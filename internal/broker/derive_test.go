package broker

import "testing"

func TestDeriveKey_DeterministicAndUnique(t *testing.T) {
	secret := []byte("broker-derive-secret")
	a := CallerID("AAAApublickeybase64rawstdxxxxxxxxxxxxxxxxxxxx")
	b := CallerID("BBBBpublickeybase64rawstdyyyyyyyyyyyyyyyyyyyy")

	if first, again := DeriveKey(secret, 1, a), DeriveKey(secret, 1, a); first != again {
		t.Fatal("same (secret,version,caller) must derive the same key")
	}
	if DeriveKey(secret, 1, a) == DeriveKey(secret, 1, b) {
		t.Fatal("different callers must derive different keys")
	}
	if DeriveKey(secret, 1, a) == DeriveKey(secret, 2, a) {
		t.Fatal("different versions must derive different keys")
	}
	if DeriveKey(secret, 1, a) == DeriveKey([]byte("other"), 1, a) {
		t.Fatal("different secrets must derive different keys")
	}
}

func TestValidateDerived_RoundTrip(t *testing.T) {
	secret := []byte("s3cr3t")
	caller := CallerID("Zm9vYmFyYmF6cXV4cHVia2V5cmF3c3RkAAAAAAAAAAAA")
	tok := DeriveKey(secret, 3, caller)

	got, ok := ValidateDerived(map[byte][]byte{3: secret}, tok)
	if !ok {
		t.Fatal("valid token must validate")
	}
	if got != string(caller) {
		t.Fatalf("caller mismatch: got %q want %q", got, caller)
	}
}

func TestValidateDerived_WrongSecretRejected(t *testing.T) {
	caller := CallerID("cGtwa3BrcGtwa3BrcGtwa3BrcGtwa3BrcGtwa3BrcGs=")
	tok := DeriveKey([]byte("real"), 1, caller)
	if _, ok := ValidateDerived(map[byte][]byte{1: []byte("attacker")}, tok); ok {
		t.Fatal("token minted with a different secret must not validate")
	}
}

func TestValidateDerived_TamperRejected(t *testing.T) {
	secret := []byte("s")
	caller := CallerID("dGFtcGVydGFtcGVydGFtcGVydGFtcGVydGFtcGVyAAAA")
	tok := DeriveKey(secret, 1, caller)

	// Flip the last MAC char to a different valid base64url char.
	b := []byte(tok)
	last := b[len(b)-1]
	if last == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	if _, ok := ValidateDerived(map[byte][]byte{1: secret}, string(b)); ok {
		t.Fatal("a tampered MAC must not validate")
	}
}

func TestValidateDerived_ForgedCallerRejected(t *testing.T) {
	secret := []byte("s")
	victim := CallerID("dmljdGltdmljdGltdmljdGltdmljdGltdmljdGltAAAA")
	tok := DeriveKey(secret, 1, victim)

	// An attacker swaps the caller segment for their own pubkey but cannot
	// recompute the MAC (no secret): re-encode with a different caller segment.
	version, _, mac, ok := ParseDerived(tok)
	if !ok {
		t.Fatal("parse")
	}
	attacker := CallerID("YXR0YWNrZXJhdHRhY2tlcmF0dGFja2VyYXR0YWNrAAAA")
	forged := "sk-smol-" + string(rune('0'+version)) + "." +
		b64.EncodeToString([]byte(attacker)) + "." + b64.EncodeToString(mac)
	if _, ok := ValidateDerived(map[byte][]byte{1: secret}, forged); ok {
		t.Fatal("swapping the caller segment must invalidate the MAC")
	}
}

func TestValidateDerived_RotationGraceWindow(t *testing.T) {
	caller := CallerID("cm90YXRpb25yb3RhdGlvbnJvdGF0aW9ucm90YXQAAAA=")
	oldSecret, newSecret := []byte("old-v1"), []byte("new-v2")

	oldTok := DeriveKey(oldSecret, 1, caller)
	newTok := DeriveKey(newSecret, 2, caller)

	// During the grace window both versions are accepted.
	grace := map[byte][]byte{1: oldSecret, 2: newSecret}
	if _, ok := ValidateDerived(grace, oldTok); !ok {
		t.Fatal("old-version token must validate during grace window")
	}
	if _, ok := ValidateDerived(grace, newTok); !ok {
		t.Fatal("new-version token must validate during grace window")
	}

	// After grace, only the current version is accepted.
	current := map[byte][]byte{2: newSecret}
	if _, ok := ValidateDerived(current, oldTok); ok {
		t.Fatal("old-version token must be rejected after the grace window")
	}
	if _, ok := ValidateDerived(current, newTok); !ok {
		t.Fatal("current-version token must still validate")
	}
}

func TestParseDerived_Malformed(t *testing.T) {
	for _, tok := range []string{
		"", "sk-smol-", "sk-smol-1", "sk-smol-1.abc", "nope.1.2.3",
		"sk-smol-x.YWJj.YWJj", "sk-smol-1..", "sk-smol-999.YWJj.YWJj",
	} {
		if _, _, _, ok := ParseDerived(tok); ok {
			t.Errorf("expected malformed token to fail parse: %q", tok)
		}
	}
}
