package broker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockUpstream asserts the broker injected the master key, then returns a cost.
func mockUpstream(t *testing.T, wantKey string) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.Header.Get("x-api-key"); got != wantKey {
			t.Errorf("upstream got x-api-key=%q, want %q (master key not injected)", got, wantKey)
		}
		if got := r.Header.Get("X-Pilot-Caller"); got != "" {
			t.Errorf("caller identity header leaked to the partner: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"cost_cents":5}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testBroker(t *testing.T, up string, quota int, now time.Time) *Broker {
	t.Helper()
	reg, err := ParseRegistry([]byte(fmt.Sprintf(
		`[{"id":"io.pilot.test","upstream":%q,"key_env":"TEST_KEY","auth_header":"x-api-key","allow":["/echo"],"quota":%d}]`, up, quota)),
		func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatal(err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	return b
}

func signedReq(t *testing.T, priv ed25519.PrivateKey, method, path string, body []byte, now time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range Sign(priv, method, path, body, now) {
		req.Header.Set(k, v)
	}
	return req
}

func TestBroker_SignedCallSucceedsAndMeters(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, hits := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now)
	_, priv := newKey(t)

	body := []byte(`{"q":"hi"}`)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", body, now))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
	caller := CallerID(Sign(priv, "POST", "/io.pilot.test/echo", body, now)[HdrCaller])
	calls, cents := b.Store.Usage("io.pilot.test", string(caller))
	if calls != 1 || cents != 5 {
		t.Fatalf("metered (%d calls, %.0f cents), want (1, 5)", calls, cents)
	}
}

func TestBroker_NoIdentityRejected(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, hits := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, httptest.NewRequest("POST", "/io.pilot.test/echo", bytes.NewReader([]byte(`{}`))))
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401 (unsigned must not reach the master key)", rec.Code)
	}
	if *hits != 0 {
		t.Fatal("unsigned request reached the upstream")
	}
}

func TestBroker_ForgedIdentityRejected(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, hits := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now)
	_, priv := newKey(t)
	// sign one path, request another → signature won't verify.
	req := signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now)
	req.URL.Path = "/io.pilot.test/echo-tampered"
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401 on tampered/forged identity", rec.Code)
	}
	if *hits != 0 {
		t.Fatal("forged request reached the upstream")
	}
}

func TestBroker_DisallowedMethodForbidden(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, hits := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now)
	_, priv := newKey(t)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/admin", []byte(`{}`), now))
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (undeclared method must not hit the upstream)", rec.Code)
	}
	if *hits != 0 {
		t.Fatal("disallowed method reached the upstream")
	}
}

func TestBroker_QuotaEnforced(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, _ := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 1, now) // quota 1
	_, priv := newKey(t)
	for i, want := range []int{200, 429} {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
		if rec.Code != want {
			t.Fatalf("call %d: status = %d, want %d", i+1, rec.Code, want)
		}
	}
}

func TestBroker_PerCallerIsolation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, _ := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 1, now) // quota 1 each
	_, a := newKey(t)
	_, bk := newKey(t)
	for _, priv := range []ed25519.PrivateKey{a, bk} {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
		if rec.Code != 200 {
			t.Fatalf("each distinct caller should get its own quota; got %d", rec.Code)
		}
	}
}

func TestBroker_UnknownApp(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, _ := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now)
	_, priv := newKey(t)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.nope/echo", []byte(`{}`), now))
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBroker_BreakerTripsOnUpstream5xx(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"partner down"}`))
	}))
	t.Cleanup(up.Close)

	reg, err := ParseRegistry([]byte(fmt.Sprintf(
		`[{"id":"io.pilot.test","upstream":%q,"key_env":"K","auth_header":"x-api-key","allow":["/echo"],"quota":0,"breaker_threshold":2}]`, up.URL)),
		func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatal(err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	_, priv := newKey(t)

	// Two calls hit the upstream, get its real 500, and trip the breaker.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
		if rec.Code != 500 {
			t.Fatalf("call %d: status = %d, want 500 (partner status passed through)", i+1, rec.Code)
		}
	}
	// Third call should fail fast (503) without hitting the upstream again.
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
	if rec.Code != 503 {
		t.Fatalf("after breaker opens, status = %d, want 503", rec.Code)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (breaker should spare the partner)", hits)
	}
}

func TestBroker_LiveRegistryReload(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, _ := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now) // registry knows only io.pilot.test
	_, priv := newKey(t)

	// A new app the broker doesn't know yet → 404.
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.added/echo", []byte(`{}`), now))
	if rec.Code != 404 {
		t.Fatalf("unknown app before reload = %d, want 404", rec.Code)
	}

	// Swap in a registry that includes it (no restart).
	reg2, err := ParseRegistry([]byte(fmt.Sprintf(
		`[{"id":"io.pilot.added","upstream":%q,"key_env":"K","auth_header":"x-api-key","allow":["/echo"],"quota":5}]`, up.URL)),
		func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatal(err)
	}
	b.SetRegistry(reg2)

	rec = httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.added/echo", []byte(`{}`), now))
	if rec.Code != 200 {
		t.Fatalf("after reload = %d, want 200 (app should be live)", rec.Code)
	}
}

func TestBroker_ConfigurableCostField(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"cost_cents":9}}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(fmt.Sprintf(
		`[{"id":"io.pilot.test","upstream":%q,"key_env":"K","auth_header":"x-api-key","allow":["/echo"],"quota":0,"cost_field":"usage.cost_cents"}]`, up.URL)),
		func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatal(err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	_, priv := newKey(t)
	body := []byte(`{}`)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", body, now))
	caller := CallerID(Sign(priv, "POST", "/io.pilot.test/echo", body, now)[HdrCaller])
	_, cents := b.Store.Usage("io.pilot.test", string(caller))
	if cents != 9 {
		t.Fatalf("metered cents = %v, want 9 (nested cost_field)", cents)
	}
}

// guard: the master key never appears in the response the caller receives.
func TestBroker_NoKeyLeakInResponse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	up, _ := mockUpstream(t, "MASTERKEY")
	b := testBroker(t, up.URL, 5, now)
	_, priv := newKey(t)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
	out, _ := io.ReadAll(rec.Body)
	if bytes.Contains(out, []byte("MASTERKEY")) {
		t.Fatal("master key leaked into the caller-visible response")
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		t.Fatalf("response not JSON: %s", out)
	}
}
