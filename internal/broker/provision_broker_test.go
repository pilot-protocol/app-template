package broker

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubCloud mimics the smol cloud contract the masterKeyProvider targets:
// /v1/artifacts stores bytes and returns a reference; /v1/machines creates an
// owner-tagged machine and lists ALL of them (no server-side filter, like the
// real cloud) so the broker's owner filter is exercised.
type stubCloud struct {
	mu       sync.Mutex
	machines []map[string]any
	failNext bool // when true, /v1/machines returns 500 once
	seq      int
}

func newStubCloud() (*stubCloud, *httptest.Server) {
	sc := &stubCloud{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		owner := r.Header.Get("X-Smol-Owner")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"reference": "tenants/T/" + owner + ":v1"})
	})
	mux.HandleFunc("/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(sc.machines)
			return
		}
		if sc.failNext {
			sc.failNext = false
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		sc.seq++
		m["id"] = "mach-" + string(rune('a'+sc.seq))
		sc.machines = append(sc.machines, m)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(m)
	})
	return sc, httptest.NewServer(mux)
}

func provTestBroker(t *testing.T, upstream string, seed, maxPerIP int) *Broker {
	t.Helper()
	regJSON := `[{
		"id":"io.pilot.smol","upstream":"` + upstream + `","key_env":"SMOL_MASTER",
		"provision":{"provider":"master","secret_env":"SMOL_SECRET","key_version":1,
			"seed_credits":` + itoa(seed) + `,"max_identities_per_ip":` + itoa(maxPerIP) + `,
			"cost_credits":{"/push":1}}
	}]`
	getenv := func(k string) string {
		switch k {
		case "SMOL_MASTER":
			return "smk_test_master"
		case "SMOL_SECRET":
			return "hmac-derive-secret"
		}
		return ""
	}
	reg, err := ParseRegistry([]byte(regJSON), getenv)
	if err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour, Now: func() time.Time { return time.Unix(10_000_000, 0) }}
	b.IPTrust = IPTrust{Header: "X-Real-IP"}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type identity struct {
	priv ed25519.PrivateKey
	pub  string // canonical base64 rawstd
}

func newIdentity(t *testing.T) identity {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return identity{priv: priv, pub: base64.RawStdEncoding.EncodeToString(pub)}
}

// call signs and dispatches a request through the broker, returning the recorder.
func (id identity) call(t *testing.T, b *Broker, method, path string, body []byte, realIP, xff string) *httptest.ResponseRecorder {
	t.Helper()
	now := b.Verify.Now()
	hdrs := Sign(id.priv, method, path, body, now)
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	if realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rr := httptest.NewRecorder()
	b.ServeHTTP(rr, req)
	return rr
}

func pushBody(name, artifact string) []byte {
	env := pushEnvelope{Name: name, Artifact: base64.StdEncoding.EncodeToString([]byte(artifact))}
	out, _ := json.Marshal(env)
	return out
}

func decodeMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return m
}

func TestProvisionedFlow_EndToEnd(t *testing.T) {
	sc, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 3, 0)
	alice := newIdentity(t)

	// provision → 200, key validates to alice, seeded 3.
	rr := alice.call(t, b, "POST", "/io.pilot.smol/_provision", nil, "1.1.1.1", "")
	if rr.Code != 200 {
		t.Fatalf("provision code %d: %s", rr.Code, rr.Body)
	}
	m := decodeMap(t, rr)
	if m["new"] != true || m["credits"].(float64) != 3 {
		t.Fatalf("provision result: %v", m)
	}
	caller, ok := ValidateDerived(map[byte][]byte{1: []byte("hmac-derive-secret")}, m["key"].(string), 0)
	if !ok || caller != alice.pub {
		t.Fatalf("derived key must validate to alice: ok=%v caller=%s", ok, caller)
	}

	// balance → 3
	rr = alice.call(t, b, "GET", "/io.pilot.smol/_balance", nil, "1.1.1.1", "")
	if decodeMap(t, rr)["credits"].(float64) != 3 {
		t.Fatalf("balance: %s", rr.Body)
	}

	// push → 201 relayed, owner tag set, balance 2
	rr = alice.call(t, b, "POST", "/io.pilot.smol/push", pushBody("web", "artifactbytes"), "1.1.1.1", "")
	if rr.Code != 201 {
		t.Fatalf("push code %d: %s", rr.Code, rr.Body)
	}
	mach := decodeMap(t, rr)
	env := mach["env"].(map[string]any)
	if env["PILOT_OWNER"] != alice.pub {
		t.Fatalf("machine must be owner-tagged with alice, got %v", env)
	}
	rr = alice.call(t, b, "GET", "/io.pilot.smol/_balance", nil, "1.1.1.1", "")
	if decodeMap(t, rr)["credits"].(float64) != 2 {
		t.Fatalf("balance after push should be 2: %s", rr.Body)
	}
	_ = sc
}

func TestProvisioned_402AtZero(t *testing.T) {
	_, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 1, 0)
	alice := newIdentity(t)
	alice.call(t, b, "POST", "/io.pilot.smol/_provision", nil, "2.2.2.2", "")
	if rr := alice.call(t, b, "POST", "/io.pilot.smol/push", pushBody("a", "x"), "2.2.2.2", ""); rr.Code != 201 {
		t.Fatalf("first push should succeed: %d %s", rr.Code, rr.Body)
	}
	if rr := alice.call(t, b, "POST", "/io.pilot.smol/push", pushBody("b", "x"), "2.2.2.2", ""); rr.Code != http.StatusPaymentRequired {
		t.Fatalf("push at zero must be 402, got %d", rr.Code)
	}
}

func TestProvisioned_RefundOnUpstream5xx(t *testing.T) {
	sc, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 2, 0)
	alice := newIdentity(t)
	alice.call(t, b, "POST", "/io.pilot.smol/_provision", nil, "3.3.3.3", "")
	sc.failNext = true
	rr := alice.call(t, b, "POST", "/io.pilot.smol/push", pushBody("a", "x"), "3.3.3.3", "")
	if rr.Code < 500 {
		t.Fatalf("upstream 500 should relay a 5xx, got %d", rr.Code)
	}
	// credit refunded → still 2
	rr = alice.call(t, b, "GET", "/io.pilot.smol/_balance", nil, "3.3.3.3", "")
	if decodeMap(t, rr)["credits"].(float64) != 2 {
		t.Fatalf("failed push must refund, balance should be 2: %s", rr.Body)
	}
}

func TestProvisioned_ListIsolation(t *testing.T) {
	_, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 5, 0)
	alice, bob := newIdentity(t), newIdentity(t)
	alice.call(t, b, "POST", "/io.pilot.smol/_provision", nil, "4.4.4.4", "")
	bob.call(t, b, "POST", "/io.pilot.smol/_provision", nil, "5.5.5.5", "")
	alice.call(t, b, "POST", "/io.pilot.smol/push", pushBody("a1", "x"), "4.4.4.4", "")
	alice.call(t, b, "POST", "/io.pilot.smol/push", pushBody("a2", "x"), "4.4.4.4", "")
	bob.call(t, b, "POST", "/io.pilot.smol/push", pushBody("b1", "x"), "5.5.5.5", "")

	var aList, bList []map[string]any
	json.Unmarshal(alice.call(t, b, "GET", "/io.pilot.smol/list", nil, "4.4.4.4", "").Body.Bytes(), &aList)
	json.Unmarshal(bob.call(t, b, "GET", "/io.pilot.smol/list", nil, "5.5.5.5", "").Body.Bytes(), &bList)
	if len(aList) != 2 || len(bList) != 1 {
		t.Fatalf("isolation broken: alice=%d bob=%d", len(aList), len(bList))
	}
	for _, m := range aList {
		if m["env"].(map[string]any)["PILOT_OWNER"] != alice.pub {
			t.Fatal("alice's list leaked another owner")
		}
	}
}

func TestProvisioned_PerIPCapIgnoresSpoofedXFF(t *testing.T) {
	_, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 1, 2) // cap = 2 identities per IP
	ip := "7.7.7.7"
	// three DISTINCT identities from the same real IP, each with a DIFFERENT
	// spoofed X-Forwarded-For — the cap must key on X-Real-IP only.
	codes := []int{}
	for i := 0; i < 3; i++ {
		id := newIdentity(t)
		rr := id.call(t, b, "POST", "/io.pilot.smol/_provision", nil, ip, "10.0.0."+itoa(i))
		codes = append(codes, rr.Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("per-IP cap should ignore spoofed XFF and trip on the 3rd: %v", codes)
	}
}

// bearerCall dispatches a cloud request authenticated by a derived KEY (no
// signature) — how an agent uses its key directly.
func bearerCall(t *testing.T, b *Broker, key, method, path string, body []byte, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	if ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	rr := httptest.NewRecorder()
	b.ServeHTTP(rr, req)
	return rr
}

func TestProvisioned_KeyRotationAndBearer(t *testing.T) {
	_, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 5, 0)
	alice := newIdentity(t)

	// provision → key K0
	k0 := decodeMap(t, alice.call(t, b, "POST", "/io.pilot.smol/_provision", nil, "1.1.1.1", ""))["key"].(string)

	// the key works as a Bearer for push (no signature)
	if rr := bearerCall(t, b, k0, "POST", "/io.pilot.smol/push", pushBody("a", "x"), "1.1.1.1"); rr.Code != 201 {
		t.Fatalf("bearer push with current key should work, got %d %s", rr.Code, rr.Body)
	}

	// a leaked key CANNOT rotate (rotate requires a signature) → 401
	if rr := bearerCall(t, b, k0, "POST", "/io.pilot.smol/_rotate", nil, "1.1.1.1"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bearer rotate must be rejected (401), got %d", rr.Code)
	}

	// the owner rotates (signed) → new key, credit preserved (was 5, one push spent = 4)
	rot := decodeMap(t, alice.call(t, b, "POST", "/io.pilot.smol/_rotate", nil, "1.1.1.1", ""))
	k1 := rot["key"].(string)
	if k1 == k0 {
		t.Fatal("rotate must return a different key")
	}
	if rot["credits"].(float64) != 4 {
		t.Fatalf("rotate must NOT reset credit, got %v", rot["credits"])
	}

	// the OLD key no longer works as a Bearer
	if rr := bearerCall(t, b, k0, "GET", "/io.pilot.smol/list", nil, "1.1.1.1"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("old key must be rejected after rotation, got %d", rr.Code)
	}
	// the NEW key works
	if rr := bearerCall(t, b, k1, "GET", "/io.pilot.smol/list", nil, "1.1.1.1"); rr.Code != 200 {
		t.Fatalf("new key must work, got %d %s", rr.Code, rr.Body)
	}
	// smol.key returns the current (rotated) key
	if got := decodeMap(t, alice.call(t, b, "GET", "/io.pilot.smol/_key", nil, "1.1.1.1", ""))["key"].(string); got != k1 {
		t.Fatalf("smol.key should return the current key")
	}
}

func TestProvisioned_UnknownMethod403(t *testing.T) {
	_, srv := newStubCloud()
	defer srv.Close()
	b := provTestBroker(t, srv.URL, 1, 0)
	alice := newIdentity(t)
	if rr := alice.call(t, b, "POST", "/io.pilot.smol/backdoor", []byte("{}"), "1.2.3.4", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("unknown provisioned method must be 403, got %d", rr.Code)
	}
}
