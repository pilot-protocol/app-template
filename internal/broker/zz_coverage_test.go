package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// --- access keys ---------------------------------------------------------

func TestAccessKeys_ParseAndCheck(t *testing.T) {
	ak := NewAccessKeys([]string{"alpha:key-one", "  ", "bare-two", "beta: key-three "})
	if ak.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (blank entry ignored)", ak.Len())
	}
	// header form
	if lbl, ok := ak.Check(hdr(map[string]string{AccessKeyHeader: "key-one"})); !ok || lbl != "alpha" {
		t.Errorf("valid header key: ok=%v label=%q", ok, lbl)
	}
	// bearer form
	if lbl, ok := ak.Check(hdr(map[string]string{"Authorization": "Bearer bare-two"})); !ok || lbl != "" {
		t.Errorf("valid bearer key: ok=%v label=%q", ok, lbl)
	}
	// label:key with spaces trimmed
	if _, ok := ak.Check(hdr(map[string]string{AccessKeyHeader: "key-three"})); !ok {
		t.Error("trimmed label:key should verify")
	}
	// wrong + missing → deny
	if _, ok := ak.Check(hdr(map[string]string{AccessKeyHeader: "nope"})); ok {
		t.Error("wrong key must not verify")
	}
	if _, ok := ak.Check(hdr(map[string]string{})); ok {
		t.Error("missing key must not verify")
	}
}

func TestAccessKeys_NoKeysFailsClosed(t *testing.T) {
	var nilAK *AccessKeys
	if _, ok := nilAK.Check(hdr(map[string]string{AccessKeyHeader: "x"})); ok {
		t.Error("nil AccessKeys must deny")
	}
	empty := NewAccessKeys([]string{"", "  "})
	if empty.Len() != 0 {
		t.Fatalf("empty Len = %d", empty.Len())
	}
	if _, ok := empty.Check(hdr(map[string]string{AccessKeyHeader: "x"})); ok {
		t.Error("no keys configured must authorize nothing")
	}
}

// TestRequireAccessKey_GatesApp: an app with require_access_key returns 401
// without a valid key and forwards with one. Also covers AppsRequiringAccessKey.
func TestRequireAccessKey_GatesApp(t *testing.T) {
	up := httptest.NewServer(okUpstream())
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.gated","upstream":"`+up.URL+`","key_env":"G_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer","allow":["/v1/x"],
		"require_access_key":true
	}]`), func(string) string { return "master" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if got := reg.AppsRequiringAccessKey(); len(got) != 1 || got[0] != "io.pilot.gated" {
		t.Fatalf("AppsRequiringAccessKey = %v", got)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	b.AccessKeys = NewAccessKeys([]string{"secret-key"})
	_, priv := newKey(t)

	// no key → 401
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.gated/v1/x", []byte(`{}`), time.Now()))
	if rec.Code != 401 {
		t.Errorf("no access key: status %d, want 401", rec.Code)
	}
	// with key → forwarded (200)
	rec = httptest.NewRecorder()
	req := signedReq(t, priv, "POST", "/io.pilot.gated/v1/x", []byte(`{}`), time.Now())
	req.Header.Set(AccessKeyHeader, "secret-key")
	b.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with access key: status %d, want 200", rec.Code)
	}
}

// --- SQLite ownership ledger ---------------------------------------------

func TestSQLiteOwnership_ClaimFirstWriterWins(t *testing.T) {
	s, err := OpenSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)

	if err := s.Claim("app", "number", "n1", "alice", now); err != nil {
		t.Fatalf("alice claim: %v", err)
	}
	if err := s.Claim("app", "number", "n1", "alice", now); err != nil {
		t.Errorf("idempotent re-claim: %v", err)
	}
	if err := s.Claim("app", "number", "n1", "mallory", now); err != ErrOwned {
		t.Errorf("takeover: %v, want ErrOwned", err)
	}
	owner, found, err := s.OwnerOf("app", "number", "n1")
	if err != nil || !found || owner != "alice" {
		t.Errorf("OwnerOf = %q,%v,%v", owner, found, err)
	}
	if _, found, _ := s.OwnerOf("app", "number", "ghost"); found {
		t.Error("unknown resource must not be found")
	}
	// OwnedSet + Release
	_ = s.Claim("app", "number", "n2", "alice", now)
	_ = s.Claim("app", "agent", "a1", "alice", now)
	set, err := s.OwnedSet("app", "number", "alice")
	if err != nil || len(set) != 2 || !set["n1"] || !set["n2"] {
		t.Errorf("OwnedSet(number) = %v (err %v)", set, err)
	}
	if err := s.Release("app", "number", "n1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.OwnerOf("app", "number", "n1"); found {
		t.Error("released resource still owned")
	}
	// Owns() helper against SQLite
	if !Owns(s, "app", "agent", "a1", "alice") || Owns(s, "app", "agent", "a1", "mallory") {
		t.Error("Owns disagrees with ledger")
	}
}

// TestSQLiteCredit_DebitRefundSettle exercises the SQLite credit ledger paths.
func TestSQLiteCredit_DebitRefundSettle(t *testing.T) {
	s, err := OpenSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)

	if _, err := s.Provision("app", "c", "1.2.3.4", 100, 0, 0, now); err != nil {
		t.Fatal(err)
	}
	if bal, err := s.Credit("app", "c"); err != nil || bal != 100 {
		t.Fatalf("seed Credit = %d,%v", bal, err)
	}
	ok, rem, err := s.Debit("app", "c", 40)
	if err != nil || !ok || rem != 60 {
		t.Fatalf("Debit 40 = %v,%d,%v", ok, rem, err)
	}
	s.Refund("app", "c", 10) // back to 70
	if bal, _ := s.Credit("app", "c"); bal != 70 {
		t.Fatalf("after refund = %d, want 70", bal)
	}
	if rem, err := s.Settle("app", "c", 1000); err != nil || rem != 0 {
		t.Fatalf("Settle overshoot = %d,%v, want clamp to 0", rem, err)
	}
	// over-debit refused
	if ok, _, _ := s.Debit("app", "c", 5); ok {
		t.Error("debit past zero must fail")
	}
	rec, found, err := s.Get("app", "c")
	if err != nil || !found || rec.Credits != 0 {
		t.Errorf("Get = %+v,%v,%v", rec, found, err)
	}
}

// --- tenancy account-summary redaction -----------------------------------

// TestFilterObject_RecomputesAndRedacts: an account-summary response has its
// count replaced by the caller's own count and its unattributable fields dropped.
func TestFilterObject_RecomputesAndRedacts(t *testing.T) {
	s := NewMemStore()
	now := time.Unix(1_800_000_000, 0)
	_ = s.Claim("app", "number", "n1", "alice", now)
	_ = s.Claim("app", "number", "n2", "alice", now)

	tn := &Tenancy{
		ParamTypes: map[string]string{"number_id": "number"},
		Create:     []CreateRoute{{Method: "POST", Path: "/v1/numbers", Type: "number", IDField: "id"}},
		Object: []ObjectRoute{{
			Method: "GET", Path: "/v1/usage",
			OwnedCounts: map[string]string{"numbers.used": "number"},
			Redact:      []string{"stats", "numbers.remaining"},
		}},
	}
	if err := validateTenancy(&AppEntry{ID: "app", Tenancy: tn}); err != nil {
		t.Fatalf("validateTenancy: %v", err)
	}
	tn.compile()

	// partner reports the whole account: 9 numbers used, remaining 991, stats blob
	raw := []byte(`{"numbers":{"used":9,"remaining":991},"stats":{"total":123}}`)
	out, did := tn.FilterObject(s, "app", "GET", "/v1/usage", raw, "alice")
	if !did {
		t.Fatal("FilterObject did not handle the route")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	nums := got["numbers"].(map[string]any)
	if fmtNum(nums["used"]) != "2" {
		t.Errorf("numbers.used = %v, want 2 (alice's own)", nums["used"])
	}
	if _, ok := nums["remaining"]; ok {
		t.Error("numbers.remaining should be redacted (derived from account-wide used)")
	}
	if _, ok := got["stats"]; ok {
		t.Error("stats should be redacted")
	}
	// a route it doesn't cover passes through untouched
	if _, did := tn.FilterObject(s, "app", "GET", "/v1/other", raw, "alice"); did {
		t.Error("uncovered route should not be handled")
	}
}

func fmtNum(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case float64:
		return json.Number(jsonNum(x)).String()
	}
	return ""
}
