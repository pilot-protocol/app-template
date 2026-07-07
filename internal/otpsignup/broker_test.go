package otpsignup

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

// mockProvider stands in for the identity provider: register → 201, verify →
// {application:{api_key}}. mockMail stands in for the otpmail control API.
func mockProvider(t *testing.T, key string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); w.Write([]byte(`{"message":"ok"}`)) })
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
			if r.Header.Get("Authorization") != "Bearer mail-tok" {
				http.Error(w, "unauth", 401)
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
		MailControlURL: mailURL, MailToken: "mail-tok", MailDomain: "mx.example.net",
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
