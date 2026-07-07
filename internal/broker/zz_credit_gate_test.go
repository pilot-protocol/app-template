package broker

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// creditBroker builds a broker whose one app has a per-caller micro-dollar budget.
// The upstream returns the given status; hits counts forwards that reached it.
func creditBroker(t *testing.T, upStatus int, now time.Time) (*Broker, *int) {
	t.Helper()
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(upStatus)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.test","upstream":"`+up.URL+`","key_env":"TEST_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer",
		"allow":["/echo","/cheap","/v1/calls/{id}"],
		"credit":{"seed_credits":100,"default_cost":10,"cost_credits":{"/echo":40,"/v1/calls/{id}":5}}
	}]`), func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	return b, &hits
}

// TestCreditGate_DebitsUntil402 exercises seeding, per-path cost, the balance
// header, exact vs default vs templated cost, and 402 on overdraw.
func TestCreditGate_DebitsUntil402(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, hits := creditBroker(t, 200, now)
	_, priv := newKey(t)

	// seed 100. /echo costs 40 → remaining 60, then 20.
	for _, want := range []int{60, 20} {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
		if rec.Code != 200 {
			t.Fatalf("/echo: status %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("X-Pilot-Credits-Remaining"); got != strconv.Itoa(want) {
			t.Fatalf("/echo remaining = %q, want %d", got, want)
		}
	}
	// third /echo needs 40 but only 20 left → 402, upstream not hit.
	before := *hits
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("overdraw: status %d, want 402 (body %s)", rec.Code, rec.Body.String())
	}
	if *hits != before {
		t.Fatal("402 call still reached the upstream")
	}
	if got := rec.Header().Get("X-Pilot-Credits-Remaining"); got != "20" {
		t.Fatalf("402 remaining = %q, want 20", got)
	}
	// /cheap uses default_cost 10 → 200, remaining 10.
	rec = httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/cheap", []byte(`{}`), now))
	if rec.Code != 200 || rec.Header().Get("X-Pilot-Credits-Remaining") != "10" {
		t.Fatalf("/cheap: %d remaining %q, want 200/10", rec.Code, rec.Header().Get("X-Pilot-Credits-Remaining"))
	}
	// templated cost: /v1/calls/{id} costs 5 → 200, remaining 5.
	rec = httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/v1/calls/call_abc", []byte(`{}`), now))
	if rec.Code != 200 || rec.Header().Get("X-Pilot-Credits-Remaining") != "5" {
		t.Fatalf("templated cost: %d remaining %q, want 200/5", rec.Code, rec.Header().Get("X-Pilot-Credits-Remaining"))
	}
}

// TestCreditGate_RefundsFailedCall: a non-2xx upstream response refunds the debit,
// so only successful calls burn budget.
func TestCreditGate_RefundsFailedCall(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, hits := creditBroker(t, 500, now)
	_, priv := newKey(t)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500 relayed", rec.Code)
	}
	if *hits != 1 {
		t.Fatal("upstream should have been hit once")
	}
	if got := rec.Header().Get("X-Pilot-Credits-Remaining"); got != "100" {
		t.Fatalf("failed call remaining = %q, want 100 (refunded)", got)
	}
}

// TestCreditGate_MutuallyExclusiveWithProvision: credit + provision is rejected.
func TestCreditGate_MutuallyExclusiveWithProvision(t *testing.T) {
	_, err := ParseRegistry([]byte(`[{"id":"io.pilot.x","upstream":"https://e.example.com","key_env":"K",
		"credit":{"seed_credits":100},"provision":{"secret_env":"S"}}]`), func(string) string { return "v" })
	if err == nil {
		t.Fatal("expected credit+provision to be rejected")
	}
}

// TestCreditGate_MethodSpecificCost: one path can be a free read AND a paid write
// (GET /v1/numbers list = free, POST /v1/numbers buy = $3). A method-scoped cost
// key prices only the write; the GET is not charged.
func TestCreditGate_MethodSpecificCost(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.test","upstream":"`+up.URL+`","key_env":"TEST_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer","allow":["/v1/numbers"],
		"credit":{"seed_credits":5000000,"default_cost":0,"cost_credits":{"POST /v1/numbers":3000000}}
	}]`), func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	_, priv := newKey(t)

	do := func(method string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, signedReq(t, priv, method, "/io.pilot.test/v1/numbers", []byte(`{}`), now))
		return rec
	}
	// GET is a free read → balance unchanged at 5000000.
	if rec := do("GET"); rec.Code != 200 || rec.Header().Get("X-Pilot-Credits-Remaining") != "5000000" {
		t.Fatalf("GET /v1/numbers: %d remaining %q, want 200/5000000 (free read)", rec.Code, rec.Header().Get("X-Pilot-Credits-Remaining"))
	}
	// POST buys a number → debit $3 → remaining $2.
	if rec := do("POST"); rec.Code != 200 || rec.Header().Get("X-Pilot-Credits-Remaining") != "2000000" {
		t.Fatalf("POST /v1/numbers: %d remaining %q, want 200/2000000 ($3 debited)", rec.Code, rec.Header().Get("X-Pilot-Credits-Remaining"))
	}
	// A second $3 buy can't fit in the remaining $2 → 402.
	if rec := do("POST"); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("second POST: %d, want 402", rec.Code)
	}
}
