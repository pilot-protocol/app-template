package publish

import (
	"strings"
	"testing"
)

func TestSafeKey(t *testing.T) {
	cases := map[string]string{
		"io.pilot.weather-0.1.0": "io.pilot.weather-0.1.0",
		"../../etc/passwd":       "____etc_passwd", // ".." and "/" both become "_"
		"a b":                    "a_b",            // spaces too
	}
	for in, want := range cases {
		if got := safeKey(in); got != want {
			t.Errorf("safeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStringHelpers(t *testing.T) {
	if indexOf("hello world", "world") != 6 {
		t.Error("indexOf basic")
	}
	if indexOf("abc", "z") != -1 {
		t.Error("indexOf absent")
	}
	if replaceAll("a.b.c", ".", "-") != "a-b-c" {
		t.Error("replaceAll")
	}
	if redact("token=SEKRET here", "SEKRET") != "token=*** here" {
		t.Error("redact")
	}
	if redact("nothing", "") != "nothing" {
		t.Error("redact empty token is a no-op")
	}
	if trimNL("line\r\n\n") != "line" {
		t.Error("trimNL")
	}
}

func TestOrenv(t *testing.T) {
	t.Setenv("PUB_TEST_VAR", "")
	if orenv("PUB_TEST_VAR", "fallback") != "fallback" {
		t.Error("orenv should fall back when unset/empty")
	}
	t.Setenv("PUB_TEST_VAR", "set")
	if orenv("PUB_TEST_VAR", "fallback") != "set" {
		t.Error("orenv should prefer the env value")
	}
}

func TestCaseStoreLifecycle(t *testing.T) {
	store, err := NewCaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := Submission{ID: "io.pilot.demo", Version: "0.1.0", Description: "demo",
		Backend: SubBackend{BaseURL: "https://api.demo.com"}}

	c, err := store.Create(sub, nil, BuildInfo{Publisher: "ed25519:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusPending {
		t.Fatalf("new case status = %q, want pending", c.Status)
	}

	got, err := store.Get(c.CaseID)
	if err != nil || got.Submission.ID != "io.pilot.demo" {
		t.Fatalf("Get round-trip failed: %v %+v", err, got)
	}

	if _, err := store.SetStatus(c.CaseID, StatusApproved, "lgtm"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(c.CaseID)
	if got.Status != StatusApproved {
		t.Fatalf("status after SetStatus = %q, want approved", got.Status)
	}
	if len(got.History) != 2 || got.History[1].Note != "lgtm" {
		t.Fatalf("history not appended: %+v", got.History)
	}

	// Re-creating an already-approved id+version is refused (bump the version).
	if _, err := store.Create(sub, nil, BuildInfo{}); err == nil {
		t.Fatal("re-creating an approved case should be refused")
	}

	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d cases (err %v), want 1", len(list), err)
	}
	if store.Dir(c.CaseID) == "" {
		t.Error("Dir should resolve a case path")
	}
}

func TestEmailBuildersIncludeKeyFields(t *testing.T) {
	sub := Submission{
		ID: "io.pilot.demo", Version: "0.1.0", Description: "A demo app",
		Backend: SubBackend{BaseURL: "https://api.demo.com"},
		Methods: []SubMethod{{Name: "demo.run", Description: "Run it"}},
		Listing: SubListing{DisplayName: "Demo"},
		Vendor:  SubVendor{Name: "Acme"},
	}

	subj, html, text := VerificationEmail("123456")
	mustHave(t, "verification", subj+html+text, "123456")

	subj, html, text = ConfirmationEmail(sub)
	mustHave(t, "confirmation", subj+html+text, "io.pilot.demo")

	subj, html, text = AcceptEmail(sub, "Find it under Demo in the store")
	mustHave(t, "accept", subj+html+text, "Demo", "Find it under Demo")

	subj, html, text = RejectEmail(sub, "needs a clearer description")
	mustHave(t, "reject", subj+html+text, "needs a clearer description")
}

func mustHave(t *testing.T, what, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("%s email missing %q", what, s)
		}
	}
}
