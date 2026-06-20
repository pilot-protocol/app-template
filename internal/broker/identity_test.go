package broker

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

// fixedClock returns a Now func pinned to t.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// hdr makes a header lookup from a map.
func hdr(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerify_ValidAccepted(t *testing.T) {
	_, priv := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"lead":{"name":"x"}}`)
	headers := Sign(priv, "POST", "/find-email", body, now)

	v := VerifyConfig{Now: fixedClock(now)}
	caller, err := v.Verify(hdr(headers), "POST", "/find-email", body)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if caller == "" || string(caller) != headers[HdrCaller] {
		t.Fatalf("caller id = %q, want the signer pubkey %q", caller, headers[HdrCaller])
	}
}

func TestVerify_MissingHeaders(t *testing.T) {
	v := VerifyConfig{Now: fixedClock(time.Unix(1_800_000_000, 0))}
	if _, err := v.Verify(hdr(map[string]string{}), "POST", "/x", nil); !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("want ErrMissingIdentity, got %v", err)
	}
}

func TestVerify_StaleTimestamp(t *testing.T) {
	_, priv := newKey(t)
	signed := time.Unix(1_800_000_000, 0)
	body := []byte("{}")
	headers := Sign(priv, "GET", "/check-balance", body, signed)
	// verify 10 minutes later with a 5-minute window → replay/stale rejected.
	v := VerifyConfig{Window: 5 * time.Minute, Now: fixedClock(signed.Add(10 * time.Minute))}
	if _, err := v.Verify(hdr(headers), "GET", "/check-balance", body); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
}

func TestVerify_TamperedBody(t *testing.T) {
	_, priv := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	headers := Sign(priv, "POST", "/find-email", []byte(`{"lead":{"name":"alice"}}`), now)
	v := VerifyConfig{Now: fixedClock(now)}
	// same headers, different body → signature must fail.
	if _, err := v.Verify(hdr(headers), "POST", "/find-email", []byte(`{"lead":{"name":"BILLIONAIRE"}}`)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature on tampered body, got %v", err)
	}
}

func TestVerify_TamperedPath(t *testing.T) {
	_, priv := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	body := []byte("{}")
	headers := Sign(priv, "POST", "/find-email", body, now)
	v := VerifyConfig{Now: fixedClock(now)}
	if _, err := v.Verify(hdr(headers), "POST", "/enrich-company", body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature on path swap, got %v", err)
	}
}

func TestVerify_ForgedSignature(t *testing.T) {
	pub, _ := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	// claim a real pubkey but provide a garbage signature.
	headers := map[string]string{
		HdrCaller:    encodeKey(pub),
		HdrTimestamp: "1800000000",
		HdrSignature: encodeSig(make([]byte, ed25519.SignatureSize)),
	}
	v := VerifyConfig{Now: fixedClock(now)}
	if _, err := v.Verify(hdr(headers), "POST", "/find-email", nil); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature on forged sig, got %v", err)
	}
}

func TestVerify_WrongKeyClaimed(t *testing.T) {
	// sign with key A but claim key B's pubkey → must fail.
	_, privA := newKey(t)
	pubB, _ := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	body := []byte("{}")
	h := Sign(privA, "POST", "/x", body, now)
	h[HdrCaller] = encodeKey(pubB)
	v := VerifyConfig{Now: fixedClock(now)}
	if _, err := v.Verify(hdr(h), "POST", "/x", body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature when claimed key != signer, got %v", err)
	}
}

func TestVerify_BadPubkey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	headers := map[string]string{HdrCaller: "not-base64-or-wrong-size", HdrTimestamp: "1800000000", HdrSignature: "AAAA"}
	v := VerifyConfig{Now: fixedClock(now)}
	if _, err := v.Verify(hdr(headers), "POST", "/x", nil); !errors.Is(err, ErrBadPubkey) {
		t.Fatalf("want ErrBadPubkey, got %v", err)
	}
}
