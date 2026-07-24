package otpsignup

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

// testMailToken is a stand-in mail-control bearer token long enough to satisfy
// the minimum-strength floor New() enforces (see minTokenLen).
const testMailToken = "test-mail-ctl-tok-32chars-long!!"

// mockProvider stands in for the identity provider: register → 201, verify →
// {application:{api_key}}. mockMail stands in for the otpmail control API.
func mockProvider(t *testing.T, key string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"message":"ok"}`))
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"application":{"api_key":"` + key + `"}}`))
	})
	return httptest.NewServer(mux)
}

func mockMail(t *testing.T, code string) *httptest.Server {
	var mu sync.Mutex
	provisioned := map[string]bool{}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testMailToken {
				http.Error(w, "unauth", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/provision", auth(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Addr string `json:"addr"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		provisioned[b.Addr] = true
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	mux.HandleFunc("/otp", auth(func(w http.ResponseWriter, r *http.Request) {
		addr := r.URL.Query().Get("addr")
		mu.Lock()
		ok := provisioned[addr]
		mu.Unlock()
		if !ok {
			w.Write([]byte(`{"ready":false}`))
			return
		}
		w.Write([]byte(`{"ready":true,"code":"` + code + `"}`)) // simulate the provider delivered the OTP
	}))
	mux.HandleFunc("/teardown", auth(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	return httptest.NewServer(mux)
}

func newBroker(t *testing.T, provURL, mailURL string) *Broker {
	t.Helper()
	b, err := New(Config{
		MailControlURL: mailURL, MailToken: testMailToken, MailDomain: "mx.example.net",
		RegisterURL: provURL + "/register", VerifyURL: provURL + "/verify",
		KeyPath: "application.api_key", OTPTimeout: 5 * time.Second, MaxIdentitiesPerIP: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestSignupMintsAndIsIdempotent(t *testing.T) {
	prov := mockProvider(t, "KEY-ABC123")
	mail := mockMail(t, "A3K9F2")
	defer prov.Close()
	defer mail.Close()
	b := newBroker(t, prov.URL, mail.URL)

	acct, err := b.Signup(context.Background(), "caller-1", "1.2.3.4")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if acct.APIKey != "KEY-ABC123" {
		t.Fatalf("api_key=%q want KEY-ABC123", acct.APIKey)
	}
	if !strings.HasPrefix(acct.Email, "pilot_") || !strings.HasSuffix(acct.Email, "@mx.example.net") {
		t.Fatalf("email=%q not pilot_*@domain", acct.Email)
	}
	if acct.Cached {
		t.Fatal("first mint should not be cached")
	}
	// idempotent repeat → same account, cached
	acct2, err := b.Signup(context.Background(), "caller-1", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !acct2.Cached || acct2.Email != acct.Email || acct2.APIKey != acct.APIKey {
		t.Fatalf("repeat not idempotent: %+v vs %+v", acct2, acct)
	}
}

func TestSignupEncryptsAtRestAndRetrieves(t *testing.T) {
	prov := mockProvider(t, "KEY-XYZ")
	mail := mockMail(t, "B4L8M1")
	defer prov.Close()
	defer mail.Close()
	b := newBroker(t, prov.URL, mail.URL)
	if _, err := b.Signup(context.Background(), "caller-2", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	// stored ciphertext must not contain the plaintext key
	var akEnc string
	if err := b.store.db.QueryRow(`SELECT apikey_enc FROM accounts WHERE caller=?`, "caller-2").Scan(&akEnc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(akEnc, "KEY-XYZ") {
		t.Fatal("api_key stored in plaintext")
	}
	rec, ok, err := b.store.get("caller-2")
	if err != nil || !ok || rec.APIKey != "KEY-XYZ" {
		t.Fatalf("retrieve after decrypt failed: ok=%v key=%q err=%v", ok, rec.APIKey, err)
	}
}

func TestPerIPCapBlocksSybil(t *testing.T) {
	prov := mockProvider(t, "K")
	mail := mockMail(t, "C1D2E3")
	defer prov.Close()
	defer mail.Close()
	b := newBroker(t, prov.URL, mail.URL) // cap = 2 per IP
	ip := "5.5.5.5"
	if _, err := b.Signup(context.Background(), "c-a", ip); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Signup(context.Background(), "c-b", ip); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Signup(context.Background(), "c-c", ip); err == nil {
		t.Fatal("expected 3rd distinct identity from one IP to be rate-limited")
	}
	// a previously-minted identity still works (idempotent, not a new mint)
	if _, err := b.Signup(context.Background(), "c-a", ip); err != nil {
		t.Fatalf("cached caller should still succeed: %v", err)
	}
}

// TestSignedHTTPFlow drives the real signed HTTP endpoint end to end.
func TestSignedHTTPFlow(t *testing.T) {
	prov := mockProvider(t, "KEY-HTTP")
	mail := mockMail(t, "F5G6H7")
	defer prov.Close()
	defer mail.Close()
	b := newBroker(t, prov.URL, mail.URL)
	ts := httptest.NewServer(http.HandlerFunc(b.handleSignup))
	defer ts.Close()

	pub, priv, _ := ed25519.GenerateKey(nil)
	_ = pub
	hdrs := broker.Sign(priv, http.MethodPost, "/signup", []byte("{}"), time.Now())
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/signup", strings.NewReader("{}"))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var acct Account
	json.NewDecoder(resp.Body).Decode(&acct)
	if acct.APIKey != "KEY-HTTP" {
		t.Fatalf("api_key=%q want KEY-HTTP", acct.APIKey)
	}

	// unsigned request → 401
	bad, _ := http.NewRequest(http.MethodPost, ts.URL+"/signup", strings.NewReader("{}"))
	r2, _ := http.DefaultClient.Do(bad)
	if r2.StatusCode != 401 {
		t.Fatalf("unsigned status=%d want 401", r2.StatusCode)
	}
}

// TestClientIPIgnoresForwardedForTrustsOnlyXRealIP is a direct, table-driven
// unit test of the trust boundary clientIP() enforces: X-Real-IP (settable
// only by our own nginx, never the client) wins when present; a client-supplied
// X-Forwarded-For is NEVER consulted, even alone; the final fallback is the raw
// socket RemoteAddr.
func TestClientIPIgnoresForwardedForTrustsOnlyXRealIP(t *testing.T) {
	mk := func(remoteAddr, realIP, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/signup", nil)
		r.RemoteAddr = remoteAddr
		if realIP != "" {
			r.Header.Set("X-Real-IP", realIP)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct {
		name                    string
		remoteAddr, realIP, xff string
		want                    string
	}{
		{"no headers falls back to RemoteAddr", "10.0.0.5:5555", "", "", "10.0.0.5"},
		{"X-Real-IP set by nginx wins", "127.0.0.1:9999", "203.0.113.9", "", "203.0.113.9"},
		{"spoofed XFF alone is ignored; RemoteAddr used", "127.0.0.1:9999", "", "1.2.3.4", "127.0.0.1"},
		{"X-Real-IP wins even with a forged XFF present", "127.0.0.1:9999", "203.0.113.9", "6.6.6.6, 7.7.7.7", "203.0.113.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clientIP(mk(c.remoteAddr, c.realIP, c.xff)); got != c.want {
				t.Errorf("clientIP()=%q want %q", got, c.want)
			}
		})
	}
}

// TestForgedXForwardedForDoesNotBypassIPCap is the HTTP-layer proof for the
// same finding: an attacker who signs each request with a FRESH identity (so
// every call looks like a distinct caller) and attaches a DIFFERENT forged
// X-Forwarded-For per request (the classic Sybil-via-spoofed-IP move) must
// still hit MaxIdentitiesPerIP, because none of these requests carry an
// X-Real-IP (only our own nginx may set that), so they all correctly bucket to
// the one real connection IP.
func TestForgedXForwardedForDoesNotBypassIPCap(t *testing.T) {
	prov := mockProvider(t, "K")
	mail := mockMail(t, "Z9Y8X7")
	defer prov.Close()
	defer mail.Close()
	b := newBroker(t, prov.URL, mail.URL) // MaxIdentitiesPerIP = 2
	ts := httptest.NewServer(http.HandlerFunc(b.handleSignup))
	defer ts.Close()

	signAndPost := func(forgedXFF string) *http.Response {
		_, priv, _ := ed25519.GenerateKey(nil) // a fresh identity every call
		hdrs := broker.Sign(priv, http.MethodPost, "/signup", []byte("{}"), time.Now())
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/signup", strings.NewReader("{}"))
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		req.Header.Set("X-Forwarded-For", forgedXFF) // attacker-controlled; must be ignored
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if r := signAndPost("9.9.9.1"); r.StatusCode != http.StatusOK {
		t.Fatalf("1st distinct identity: status=%d want 200", r.StatusCode)
	}
	if r := signAndPost("9.9.9.2"); r.StatusCode != http.StatusOK {
		t.Fatalf("2nd distinct identity: status=%d want 200", r.StatusCode)
	}
	// cap is 2: a 3rd distinct identity — with yet another forged XFF, exactly
	// what a Sybil attacker would vary per request — must still be rejected.
	if r := signAndPost("9.9.9.3"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd distinct identity with forged XFF: status=%d want 429 (a spoofed X-Forwarded-For must not bypass MaxIdentitiesPerIP)", r.StatusCode)
	}
}

// TestConcurrentSignupsForSameCallerMintOnlyOneAccount is the fix for the
// signup race: N goroutines calling Signup for the SAME caller concurrently
// must serialize on the per-caller lock, mint the provider account exactly
// once, and all return the identical (cached) account — never a duplicate.
func TestConcurrentSignupsForSameCallerMintOnlyOneAccount(t *testing.T) {
	var registerCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registerCalls, 1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"message":"ok"}`))
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"application":{"api_key":"KEY-RACE"}}`))
	})
	prov := httptest.NewServer(mux)
	defer prov.Close()
	mail := mockMail(t, "R4C3O1")
	defer mail.Close()
	b := newBroker(t, prov.URL, mail.URL)

	const n = 8
	var wg sync.WaitGroup
	accts := make([]*Account, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			accts[i], errs[i] = b.Signup(context.Background(), "same-caller", "6.6.6.6")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Signup: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if accts[i].Email != accts[0].Email || accts[i].APIKey != accts[0].APIKey {
			t.Fatalf("goroutine %d minted a different account: %+v vs %+v", i, accts[i], accts[0])
		}
	}
	if got := atomic.LoadInt32(&registerCalls); got != 1 {
		t.Fatalf("provider /register called %d times, want exactly 1 (concurrent signups for one caller must serialize, not race)", got)
	}
}

// TestNewRejectsWeakMailToken guards the "enforce a strong token" mitigation
// for the (still bearer-auth-only, no mTLS) broker<->mail control plane.
func TestNewRejectsWeakMailToken(t *testing.T) {
	_, err := New(Config{
		MailControlURL: "http://127.0.0.1:1", MailToken: "short", MailDomain: "mx.example.net",
		RegisterURL: "https://example.com/register", VerifyURL: "https://example.com/verify",
	})
	if err == nil {
		t.Fatal("expected New to reject a MailToken shorter than minTokenLen")
	}
}
