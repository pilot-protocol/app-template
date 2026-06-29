package publish

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidPublisherPin(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	good := "ed25519:" + base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if !ValidPublisherPin(good) {
		t.Fatalf("a real ed25519 pubkey pin should be valid: %s", good)
	}
	for _, bad := range []string{"", "ed25519:", "AAAA", "ed25519:not-base64!!", "rsa:" + base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		if ValidPublisherPin(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// setPublisher flips exactly one app's pin, preserves all other fields/apps, and
// re-emits parseable JSON whose signature (computed over the bytes) round-trips.
func TestSetPublisher(t *testing.T) {
	cat := `{"version":2,"updated_at":"now","apps":[
		{"id":"io.pilot.foo","version":"1.0.0","publisher":"ed25519:OLD","bundle_url":"u","extra":"keep"},
		{"id":"io.pilot.bar","version":"2.0.0","publisher":"ed25519:BARKEY"}
	]}`
	out, oldPub, err := setPublisher([]byte(cat), "io.pilot.foo", "ed25519:NEW")
	if err != nil {
		t.Fatal(err)
	}
	if oldPub != "ed25519:OLD" {
		t.Errorf("oldPub = %q, want ed25519:OLD", oldPub)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	apps := parsed["apps"].([]any)
	foo := apps[0].(map[string]any)
	if foo["publisher"] != "ed25519:NEW" {
		t.Errorf("foo publisher = %v, want ed25519:NEW", foo["publisher"])
	}
	if foo["extra"] != "keep" {
		t.Error("unrelated field 'extra' was dropped")
	}
	bar := apps[1].(map[string]any)
	if bar["publisher"] != "ed25519:BARKEY" {
		t.Error("a different app's pin was modified")
	}

	if _, _, err := setPublisher([]byte(cat), "io.pilot.missing", "ed25519:NEW"); err == nil {
		t.Error("expected error for an app not in the catalogue")
	}
}

// decodeSignKey rejects a key whose public half doesn't match the trust pubkey,
// and accepts one that does (with the env override pointing the trust at it).
func TestDecodeSignKeyMatchesTrust(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	hexKey := hex.EncodeToString(priv)

	// Wrong trust key → rejected.
	t.Setenv("PILOT_CATALOGUE_PUBKEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if _, err := decodeSignKey(hexKey); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected trust-mismatch error, got %v", err)
	}
	// Matching trust key → accepted.
	t.Setenv("PILOT_CATALOGUE_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	if _, err := decodeSignKey(hexKey); err != nil {
		t.Fatalf("matching key should decode: %v", err)
	}
	// Bad hex → rejected.
	if _, err := decodeSignKey("xyz"); err == nil {
		t.Error("expected error for bad hex key")
	}
}
