package insforgesignup

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

// mockInsforge stands in for the InsForge platform API: OAuth refresh → an
// access token, create-project → a project, access-api-key → the ik_ key. When
// limit is true, create-project returns the free-plan project cap error.
func mockInsforge(t *testing.T, key string, limit bool) (*httptest.Server, *int32) {
	t.Helper()
	var created int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/oauth/v1/token", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"at_test","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/organizations/v1/org-1/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at_test" {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		if limit {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":"project.limit.reached","error":"Free plan allows up to 2 active projects."}`))
			return
		}
		atomic.AddInt32(&created, 1)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"project":{"id":"proj-1","appkey":"abc123","region":"us-east"}}`))
	})
	mux.HandleFunc("/projects/v1/proj-1/access-api-key", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_api_key":"` + key + `"}`))
	})
	return httptest.NewServer(mux), &created
}

func newBroker(t *testing.T, api string) *Broker {
	t.Helper()
	b, err := New(Config{
		TokenURL: api + "/api/oauth/v1/token", ClientID: "clf_test", RefreshToken: "ref_test",
		PlatformAPI: api, OrgID: "org-1", MaxIdentitiesPerIP: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestSignupProvisionsAndIsIdempotent(t *testing.T) {
	srv, created := mockInsforge(t, "ik_ABC123", false)
	defer srv.Close()
	b := newBroker(t, srv.URL)

	acct, err := b.Signup(context.Background(), "caller-1", "1.2.3.4")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if acct.APIKey != "ik_ABC123" {
		t.Fatalf("api_key=%q want ik_ABC123", acct.APIKey)
	}
	if acct.BackendURL != "https://abc123.us-east.insforge.app" {
		t.Fatalf("backend_url=%q unexpected", acct.BackendURL)
	}
	if acct.ProjectID != "proj-1" || acct.Cached {
		t.Fatalf("unexpected acct=%+v", acct)
	}
	// idempotent repeat → same account, cached, NO second project created.
	acct2, err := b.Signup(context.Background(), "caller-1", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !acct2.Cached || acct2.APIKey != acct.APIKey || acct2.BackendURL != acct.BackendURL {
		t.Fatalf("repeat not idempotent: %+v vs %+v", acct2, acct)
	}
	if n := atomic.LoadInt32(created); n != 1 {
		t.Fatalf("created %d projects, want exactly 1 (idempotent)", n)
	}
}

func TestSignupEncryptsKeyAtRest(t *testing.T) {
	srv, _ := mockInsforge(t, "ik_SECRET", false)
	defer srv.Close()
	b := newBroker(t, srv.URL)
	if _, err := b.Signup(context.Background(), "caller-2", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	var akEnc string
	if err := b.store.db.QueryRow(`SELECT apikey_enc FROM projects WHERE caller=?`, "caller-2").Scan(&akEnc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(akEnc, "ik_SECRET") {
		t.Fatal("api_key stored in plaintext")
	}
	rec, ok, err := b.store.get("caller-2")
	if err != nil || !ok || rec.APIKey != "ik_SECRET" {
		t.Fatalf("retrieve after decrypt failed: ok=%v key=%q err=%v", ok, rec.APIKey, err)
	}
}

func TestPerIPCapBlocksSybil(t *testing.T) {
	srv, _ := mockInsforge(t, "ik_K", false)
	defer srv.Close()
	b := newBroker(t, srv.URL) // cap = 2 per IP
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
	if _, err := b.Signup(context.Background(), "c-a", ip); err != nil {
		t.Fatalf("cached caller should still succeed: %v", err)
	}
}

func TestProjectLimitError(t *testing.T) {
	srv, _ := mockInsforge(t, "ik_X", true) // create-project returns the cap error
	defer srv.Close()
	b := newBroker(t, srv.URL)
	_, err := b.Signup(context.Background(), "caller-x", "2.2.2.2")
	if err == nil || !strings.Contains(err.Error(), "project limit") {
		t.Fatalf("want project-limit error, got %v", err)
	}
}

// TestSignedHTTPFlow drives the real signed HTTP endpoint end to end.
func TestSignedHTTPFlow(t *testing.T) {
	srv, _ := mockInsforge(t, "ik_HTTP", false)
	defer srv.Close()
	b := newBroker(t, srv.URL)
	ts := httptest.NewServer(http.HandlerFunc(b.handleSignup))
	defer ts.Close()

	_, priv, _ := ed25519.GenerateKey(nil)
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
	if acct.APIKey != "ik_HTTP" {
		t.Fatalf("api_key=%q want ik_HTTP", acct.APIKey)
	}

	// unsigned request → 401
	bad, _ := http.NewRequest(http.MethodPost, ts.URL+"/signup", strings.NewReader("{}"))
	r2, _ := http.DefaultClient.Do(bad)
	if r2.StatusCode != 401 {
		t.Fatalf("unsigned status=%d want 401", r2.StatusCode)
	}
}
