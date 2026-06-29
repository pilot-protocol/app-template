package catalogue

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogueURL(t *testing.T) {
	t.Setenv("PILOT_CATALOGUE_URL", "")
	if got := CatalogueURL(); got != DefaultCatalogueURL {
		t.Errorf("default = %q, want %q", got, DefaultCatalogueURL)
	}
	t.Setenv("PILOT_CATALOGUE_URL", "file:///tmp/x.json")
	if got := CatalogueURL(); got != "file:///tmp/x.json" {
		t.Errorf("override = %q", got)
	}
}

// writeSignedCatalogue writes body + a detached .sig signed by priv and returns
// the file:// URL fetch() will read.
func writeSignedCatalogue(t *testing.T, body []byte, priv ed25519.PrivateKey) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "catalogue.json")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))
	if err := os.WriteFile(p+".sig", []byte(sig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "file://" + p
}

func TestFetchOwnersVerifiesAndMaps(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PILOT_CATALOGUE_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	body := []byte(`{"version":2,"apps":[` +
		`{"id":"io.pilot.a","version":"1.2.0","publisher":"ed25519:KEYA"},` +
		`{"id":"io.pilot.b","version":"0.1.0","publisher":"ed25519:KEYB"}]}`)
	url := writeSignedCatalogue(t, body, priv)

	owners, err := FetchOwners(url)
	if err != nil {
		t.Fatalf("FetchOwners: %v", err)
	}
	if len(owners) != 2 {
		t.Fatalf("want 2 owners, got %d", len(owners))
	}
	if owners["io.pilot.a"].Version != "1.2.0" || owners["io.pilot.a"].Publisher != "ed25519:KEYA" {
		t.Errorf("owner a = %+v", owners["io.pilot.a"])
	}
}

func TestFetchOwnersRejectsBadSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PILOT_CATALOGUE_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	body := []byte(`{"version":2,"apps":[]}`)
	url := writeSignedCatalogue(t, body, priv)

	// Tamper the catalogue bytes AFTER signing — the signature no longer matches.
	path := strings.TrimPrefix(url, "file://")
	if err := os.WriteFile(path, []byte(`{"version":2,"apps":[{"id":"io.pilot.evil","version":"9.9.9","publisher":"ed25519:EVIL"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchOwners(url); err == nil {
		t.Error("FetchOwners must reject a catalogue whose signature does not verify")
	}
}

func TestFetchOwnersSignatureDisabled(t *testing.T) {
	t.Setenv("PILOT_CATALOGUE_PUBKEY", "-") // sentinel disables verification (local only)
	p := filepath.Join(t.TempDir(), "catalogue.json")
	if err := os.WriteFile(p, []byte(`{"version":2,"apps":[{"id":"io.pilot.x","version":"1.0.0","publisher":"ed25519:K"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	owners, err := FetchOwners("file://" + p)
	if err != nil {
		t.Fatalf("disabled-sig fetch should succeed: %v", err)
	}
	if owners["io.pilot.x"].Version != "1.0.0" {
		t.Errorf("owners = %+v", owners)
	}
}
