package publish

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestR2FromEnvDefaultsAndOverrides(t *testing.T) {
	// Only an account id set → endpoint derived, other fields defaulted.
	t.Setenv("R2_ENDPOINT", "")
	t.Setenv("R2_ACCOUNT_ID", "acct123")
	t.Setenv("R2_REGION", "")
	t.Setenv("R2_BUCKET", "")
	t.Setenv("R2_PUBLIC_BASE", "")
	t.Setenv("R2_ACCESS_KEY_ID", "")
	t.Setenv("R2_SECRET_ACCESS_KEY", "")
	c := R2FromEnv()
	if c.Endpoint != "https://acct123.r2.cloudflarestorage.com" {
		t.Errorf("derived endpoint = %q", c.Endpoint)
	}
	if c.Region != "auto" || c.Bucket != "pilot-artifacts-prod" {
		t.Errorf("defaults wrong: %+v", c)
	}
	if c.PublicBase != "https://artifacts.pilotprotocol.network" {
		t.Errorf("public base = %q", c.PublicBase)
	}
	if c.Configured() {
		t.Error("must be unconfigured without access/secret keys")
	}

	// Explicit overrides, with trailing slashes trimmed.
	t.Setenv("R2_ENDPOINT", "https://x.example.com/")
	t.Setenv("R2_BUCKET", "mybucket")
	t.Setenv("R2_REGION", "us-east-1")
	t.Setenv("R2_PUBLIC_BASE", "https://cdn.example.com/")
	t.Setenv("R2_ACCESS_KEY_ID", "AK")
	t.Setenv("R2_SECRET_ACCESS_KEY", "SK")
	c = R2FromEnv()
	if c.Endpoint != "https://x.example.com" {
		t.Errorf("endpoint trailing slash not trimmed: %q", c.Endpoint)
	}
	if c.Bucket != "mybucket" || c.Region != "us-east-1" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.PublicBase != "https://cdn.example.com" {
		t.Errorf("public base trailing slash not trimmed: %q", c.PublicBase)
	}
	if !c.Configured() {
		t.Error("must be configured with endpoint + keys")
	}
}

func TestPresignPutStructure(t *testing.T) {
	c := R2Config{Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "b", AccessKey: "AK", SecretKey: "SK", Region: "auto"}
	signed, err := c.PresignPut("io.pilot.x/1.0.0/linux-amd64/app", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Errorf("algorithm = %q", q.Get("X-Amz-Algorithm"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("missing X-Amz-Signature")
	}
	if !strings.HasPrefix(q.Get("X-Amz-Credential"), "AK/") {
		t.Errorf("credential = %q", q.Get("X-Amz-Credential"))
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Errorf("expires = %q, want 300", q.Get("X-Amz-Expires"))
	}
	if !strings.Contains(u.Path, "/b/io.pilot.x/1.0.0/linux-amd64/app") {
		t.Errorf("path-style key missing: %q", u.Path)
	}
}

func TestObjectExists(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := R2Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK", Region: "auto"}

	status = http.StatusOK
	if ok, err := c.ObjectExists("k"); err != nil || !ok {
		t.Errorf("200 should report exists: ok=%v err=%v", ok, err)
	}
	status = http.StatusNotFound
	if ok, err := c.ObjectExists("k"); err != nil || ok {
		t.Errorf("404 should report not-exists: ok=%v err=%v", ok, err)
	}
	status = http.StatusInternalServerError
	if _, err := c.ObjectExists("k"); err == nil {
		t.Error("500 should return an error")
	}
}
