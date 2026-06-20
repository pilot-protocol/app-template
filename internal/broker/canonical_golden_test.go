package broker

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

// TestCanonicalGolden pins the exact canonical byte string the broker signs and
// verifies. The generated managed adapter (internal/scaffold/templates/
// signer.go.tmpl) inlines an IDENTICAL canonical; if either side changes this
// string, that adapter's signatures stop verifying. Both ends are locked: this
// golden test on the broker side, and a string-match assertion on the template
// side (TestManagedGeneratesKeylessSigningAdapter).
func TestCanonicalGolden(t *testing.T) {
	method, path, ts := "POST", "/io.pilot.sixtyfour/enrich", "1800000000"
	body := []byte(`{"q":"acme"}`)

	sum := sha256.Sum256(body)
	want := method + "\n" + path + "\n" + ts + "\n" + base64.RawStdEncoding.EncodeToString(sum[:])

	if got := string(canonical(method, path, ts, body)); got != want {
		t.Fatalf("canonical drift:\n got %q\nwant %q", got, want)
	}
}

// TestAdapterStyleSignatureVerifies proves a signature produced exactly the way
// the generated adapter produces it (canonical + RawStd base64 headers) verifies
// against the broker. This is the adapter↔broker contract, end to end.
func TestAdapterStyleSignatureVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	method, path := "GET", "/io.pilot.sixtyfour/find-email"
	var body []byte // GET: empty body, as the adapter sends

	// Reproduce the adapter's signer.go byte-for-byte.
	ts := "1800000000"
	sum := sha256.Sum256(body)
	canon := method + "\n" + path + "\n" + ts + "\n" + base64.RawStdEncoding.EncodeToString(sum[:])
	sig := ed25519.Sign(priv, []byte(canon))
	headers := map[string]string{
		"X-Pilot-Caller":    base64.RawStdEncoding.EncodeToString(pub),
		"X-Pilot-Timestamp": ts,
		"X-Pilot-Signature": base64.RawStdEncoding.EncodeToString(sig),
	}

	caller, err := VerifyConfig{Now: func() time.Time { return now }}.
		Verify(func(k string) string { return headers[k] }, method, path, body)
	if err != nil {
		t.Fatalf("adapter-style signature failed broker verification: %v", err)
	}
	if string(caller) != base64.RawStdEncoding.EncodeToString(pub) {
		t.Fatalf("verified caller = %q, want the signer pubkey", caller)
	}
}
