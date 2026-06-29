package catalogue

import "testing"

func ownerSet() map[string]Owner {
	return map[string]Owner{
		"io.pilot.foo": {Version: "1.2.0", Publisher: "ed25519:OWNERKEYAAA"},
	}
}

// A brand-new id is a first publish: no version/publisher constraint.
func TestCheckUpdate_NewApp(t *testing.T) {
	r := CheckUpdate(ownerSet(), "io.pilot.brandnew", "0.1.0", "ed25519:ANYKEY")
	if !r.OK() {
		t.Fatalf("new app should pass, got %+v", r.Checks)
	}
}

// Same owner key + higher version passes.
func TestCheckUpdate_ValidUpdate(t *testing.T) {
	r := CheckUpdate(ownerSet(), "io.pilot.foo", "1.3.0", "ed25519:OWNERKEYAAA")
	if !r.OK() {
		t.Fatalf("valid update should pass, got %+v", r.Checks)
	}
}

// A different signing key is rejected (the core hijack defense).
func TestCheckUpdate_WrongKey(t *testing.T) {
	r := CheckUpdate(ownerSet(), "io.pilot.foo", "1.3.0", "ed25519:ATTACKERBBB")
	if r.OK() {
		t.Fatal("update signed by a non-owner key must fail")
	}
	if !failed(r, "signed by the owning publisher") {
		t.Fatalf("expected the publisher check to fail, got %+v", r.Checks)
	}
}

// Same version (re-publish) and downgrade are both rejected.
func TestCheckUpdate_NotHigher(t *testing.T) {
	for _, v := range []string{"1.2.0", "1.1.9", "0.9.0"} {
		r := CheckUpdate(ownerSet(), "io.pilot.foo", v, "ed25519:OWNERKEYAAA")
		if r.OK() {
			t.Fatalf("version %s is not an increase over 1.2.0 — must fail", v)
		}
	}
}

// The rich form path (publisher unknown at gate time) still enforces the version
// increase and defers the key check to server-side signing.
func TestCheckUpdate_RichPathDefersKey(t *testing.T) {
	r := CheckUpdate(ownerSet(), "io.pilot.foo", "1.3.0", "")
	if !r.OK() {
		t.Fatalf("rich path with higher version should pass, got %+v", r.Checks)
	}
}

func failed(r Result, name string) bool {
	for _, c := range r.Checks {
		if c.Name == name && !c.OK {
			return true
		}
	}
	return false
}
