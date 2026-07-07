package broker

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// balanceOf issues a signed GET to the canonical credit-balance route and returns
// the decoded credits_remaining plus the raw recorder.
func balanceOf(t *testing.T, b *Broker, priv ed25519.PrivateKey, now time.Time) (int, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "GET", "/io.pilot.test/_pilot/balance", nil, now))
	var out struct {
		Balance          string `json:"balance"`
		CreditsRemaining int    `json:"credits_remaining"`
		CreditsSeed      int    `json:"credits_seed"`
		Unit             string `json:"unit"`
		Scope            string `json:"scope"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code == http.StatusOK {
		if out.Unit != "micro_usd" || out.Scope != "per-pilot-user" {
			t.Fatalf("balance unit/scope = %q/%q, want micro_usd/per-pilot-user (body %s)", out.Unit, out.Scope, rec.Body.String())
		}
	}
	return out.CreditsRemaining, rec
}

// TestBalanceMeta_CanonicalFreeReadReflectsSpend verifies the canonical
// /_pilot/balance route: it seeds a fresh caller, reflects debits, never touches
// the upstream, and never charges — all without any balance_path config.
func TestBalanceMeta_CanonicalFreeReadReflectsSpend(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, hits := creditBroker(t, 200, now) // credit app, no balance_path set
	_, priv := newKey(t)

	// 1. First-ever call is the balance check itself → caller seeded to 100,
	//    reported, upstream never hit, header set.
	bal, rec := balanceOf(t, b, priv, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("balance status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if bal != 100 {
		t.Fatalf("seeded balance = %d, want 100", bal)
	}
	if rec.Header().Get("X-Pilot-Credits-Remaining") != "100" {
		t.Fatalf("balance header = %q, want 100", rec.Header().Get("X-Pilot-Credits-Remaining"))
	}
	if *hits != 0 {
		t.Fatalf("balance check hit the upstream %d times, want 0", *hits)
	}

	// 2. Spend 40 on /echo → remaining 60.
	spend := httptest.NewRecorder()
	b.ServeHTTP(spend, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
	if spend.Code != http.StatusOK {
		t.Fatalf("/echo status %d, want 200", spend.Code)
	}

	// 3. Balance reflects the spend, still free and still no upstream hit.
	hitsAfterSpend := *hits
	for i := 0; i < 3; i++ {
		bal, rec = balanceOf(t, b, priv, now)
		if rec.Code != http.StatusOK || bal != 60 {
			t.Fatalf("post-spend balance = %d (status %d), want 60", bal, rec.Code)
		}
	}
	if *hits != hitsAfterSpend {
		t.Fatal("balance check debited/forwarded — it must be a pure read")
	}
}
