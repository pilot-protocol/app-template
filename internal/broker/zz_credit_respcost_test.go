package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// respCostBroker builds a broker whose one app meters by RESPONSE cost: only the
// billable path (POST /v1/run) costs money, and the amount is read from the
// upstream response `priceCents` (real cents) × cost_scale (10000 micro-$ / cent).
// The upstream echoes a fixed priceCents so tests can assert the settled debit.
func respCostBroker(t *testing.T, priceCents, upStatus, seed int) (*Broker, *int) {
	t.Helper()
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(upStatus)
		_, _ = w.Write([]byte(`{"success":true,"priceCents":` + strconv.Itoa(priceCents) + `,"data":{"ok":true}}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.orthogonal","upstream":"`+up.URL+`","key_env":"ORTH_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer",
		"cost_field":"priceCents",
		"allow":["/v1/search","/v1/run"],
		"credit":{
			"seed_credits":`+strconv.Itoa(seed)+`,"default_cost":0,
			"cost_source":"response","cost_scale":10000,
			"cost_credits":{"POST /v1/run":1}
		}
	}]`), func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(time.Unix(1_800_000_000, 0))}
	return b, &hits
}

// TestRespCost_SettlesActualFromResponse: a $5 budget, a run whose response says
// priceCents:1 ($0.01), debits exactly 10000 micro-$ — the real cost, not a guess.
func TestRespCost_SettlesActualFromResponse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, hits := respCostBroker(t, 1, 200, 5_000_000)
	_, priv := newKey(t)

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{"api":"olostep","path":"/v1/scrapes"}`), now))
	if rec.Code != 200 {
		t.Fatalf("run: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
	if got := rec.Header().Get("X-Pilot-Credits-Remaining"); got != "4990000" {
		t.Fatalf("remaining = %q, want 4990000 ($5 − $0.01)", got)
	}
}

// TestRespCost_FreeControlPlaneAlwaysPasses: an unlisted-cost path (/v1/search,
// default_cost 0) is free — never debited, and still allowed after the budget is
// depleted (so a broke user can still discover/price endpoints).
func TestRespCost_FreeControlPlaneAlwaysPasses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, _ := respCostBroker(t, 1, 200, 10_000) // exactly one $0.01 run of budget
	_, priv := newKey(t)

	// Free search does not debit the budget.
	s1 := httptest.NewRecorder()
	b.ServeHTTP(s1, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/search", []byte(`{"prompt":"find email"}`), now))
	if s1.Code != 200 || s1.Header().Get("X-Pilot-Credits-Remaining") != "10000" {
		t.Fatalf("free /v1/search: %d remaining %q, want 200/10000 (unchanged)", s1.Code, s1.Header().Get("X-Pilot-Credits-Remaining"))
	}
	// Deplete via a billable run.
	run := httptest.NewRecorder()
	b.ServeHTTP(run, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if run.Code != 200 || run.Header().Get("X-Pilot-Credits-Remaining") != "0" {
		t.Fatalf("run: %d remaining %q, want 200/0", run.Code, run.Header().Get("X-Pilot-Credits-Remaining"))
	}
	// Search still works at zero balance.
	s2 := httptest.NewRecorder()
	b.ServeHTTP(s2, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/search", []byte(`{"prompt":"more"}`), now))
	if s2.Code != 200 {
		t.Fatalf("free /v1/search at zero balance: status %d, want 200", s2.Code)
	}
}

// TestRespCost_402WhenDepleted: once the budget hits zero, a billable run is
// refused with 402 before the master key is ever used.
func TestRespCost_402WhenDepleted(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// seed exactly one $0.01 call; the second must 402.
	b, hits := respCostBroker(t, 1, 200, 10_000)
	_, priv := newKey(t)

	first := httptest.NewRecorder()
	b.ServeHTTP(first, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if first.Code != 200 || first.Header().Get("X-Pilot-Credits-Remaining") != "0" {
		t.Fatalf("first run: %d remaining %q, want 200/0", first.Code, first.Header().Get("X-Pilot-Credits-Remaining"))
	}
	before := *hits
	second := httptest.NewRecorder()
	b.ServeHTTP(second, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if second.Code != http.StatusPaymentRequired {
		t.Fatalf("depleted run: status %d, want 402", second.Code)
	}
	if *hits != before {
		t.Fatal("402 run still reached the upstream / master key")
	}
}

// TestRespCost_OvershootClampsToZero: a single call costing more than the balance
// is allowed (price unknown up front) but the debit clamps the balance to zero —
// never negative — and the next billable call 402s.
func TestRespCost_OvershootClampsToZero(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// balance $0.005 (5000 micro-$), a $0.01 call (10000 micro-$) overshoots.
	b, _ := respCostBroker(t, 1, 200, 5_000)
	_, priv := newKey(t)

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if rec.Code != 200 || rec.Header().Get("X-Pilot-Credits-Remaining") != "0" {
		t.Fatalf("overshoot: %d remaining %q, want 200/0 (clamped, not negative)", rec.Code, rec.Header().Get("X-Pilot-Credits-Remaining"))
	}
	next := httptest.NewRecorder()
	b.ServeHTTP(next, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if next.Code != http.StatusPaymentRequired {
		t.Fatalf("after overshoot: status %d, want 402", next.Code)
	}
}

// TestRespCost_FailedCallIsFree: a non-2xx run burns no budget (no up-front debit
// in response mode, nothing settled on failure).
func TestRespCost_FailedCallIsFree(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, hits := respCostBroker(t, 10, 500, 5_000_000)
	_, priv := newKey(t)

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500 relayed", rec.Code)
	}
	if *hits != 1 {
		t.Fatal("upstream should have been hit once")
	}
	if got := rec.Header().Get("X-Pilot-Credits-Remaining"); got != "5000000" {
		t.Fatalf("failed run remaining = %q, want 5000000 (unbilled)", got)
	}
}

// TestCreditBalance_PerUserNotAccount: the broker answers the balance path from its
// OWN ledger and never forwards it, so the shared account balance is never exposed —
// the caller sees only their own remaining budget, which reflects debits.
func TestCreditBalance_PerUserNotAccount(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var upHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upHits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"priceCents":1,"balance":"$54321.00"}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.orthogonal","upstream":"`+up.URL+`","key_env":"ORTH_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer","cost_field":"priceCents",
		"allow":["/v1/run"],
		"credit":{"seed_credits":5000000,"default_cost":0,"cost_source":"response","cost_scale":10000,
			"cost_credits":{"POST /v1/run":1},"balance_path":"/v1/credits/balance"}
	}]`), func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	_, priv := newKey(t)

	balance := func() map[string]any {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, signedReq(t, priv, "GET", "/io.pilot.orthogonal/v1/credits/balance", nil, now))
		if rec.Code != 200 {
			t.Fatalf("balance: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("balance decode: %v", err)
		}
		return m
	}

	// Fresh caller → seeded $5, and the upstream (which would leak "$54321.00") is
	// NOT contacted.
	m := balance()
	if m["balance"] != "$5.00" || m["scope"] != "per-pilot-user" {
		t.Fatalf("balance = %v, want $5.00 / per-pilot-user", m)
	}
	if upHits != 0 {
		t.Fatal("balance path was forwarded upstream — account balance could leak")
	}
	// Spend $0.01 via a run, then balance reflects the per-user debit.
	run := httptest.NewRecorder()
	b.ServeHTTP(run, signedReq(t, priv, "POST", "/io.pilot.orthogonal/v1/run", []byte(`{}`), now))
	if run.Code != 200 {
		t.Fatalf("run: %d", run.Code)
	}
	if m := balance(); m["balance"] != "$4.99" {
		t.Fatalf("post-spend balance = %v, want $4.99", m["balance"])
	}
	if upHits != 1 { // only the run hit upstream; neither balance call did
		t.Fatalf("upstream hits = %d, want 1 (run only)", upHits)
	}
}

// TestCreditPath_PerIPIdentityCap: with max_identities_per_ip=1, a second DISTINCT
// pilot identity from the same source IP cannot claim a fresh budget — this is the
// anti-Sybil guard that stops farming new $5 grants after depletion. Both callers
// share httptest's default RemoteAddr (192.0.2.1).
func TestCreditPath_PerIPIdentityCap(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.orthogonal","upstream":"`+up.URL+`","key_env":"ORTH_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer","allow":["/v1/search"],
		"credit":{"seed_credits":5000000,"default_cost":0,"max_identities_per_ip":1}
	}]`), func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}

	_, priv1 := newKey(t)
	rec1 := httptest.NewRecorder()
	b.ServeHTTP(rec1, signedReq(t, priv1, "POST", "/io.pilot.orthogonal/v1/search", []byte(`{}`), now))
	if rec1.Code != 200 {
		t.Fatalf("first identity: status %d, want 200", rec1.Code)
	}
	// A different key = a different pilot identity from the same IP → capped.
	_, priv2 := newKey(t)
	rec2 := httptest.NewRecorder()
	b.ServeHTTP(rec2, signedReq(t, priv2, "POST", "/io.pilot.orthogonal/v1/search", []byte(`{}`), now))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second identity same IP: status %d, want 429 (IP cap)", rec2.Code)
	}
}
