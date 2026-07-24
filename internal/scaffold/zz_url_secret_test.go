package scaffold

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

const urlSecretSpec = `
id: io.pilot.dyn
app_version: 0.1.0
description: "App whose backend is provisioned per user at signup."
backend:
  type: http
  base_url: https://api.example.com
  auth: byo
  url_secret: DYN_BACKEND_URL
  headers:
    Authorization: "Bearer ${DYN_API_KEY}"
methods:
  - name: dyn.signup
    summary: "Provision your backend."
    duration: slow
    signup: {step: broker, broker_url: "https://broker.example/dyn/signup", secret_key: DYN_API_KEY}
  - name: dyn.account
    summary: "Read the cached account."
    duration: fast
    signup: {step: account, secret_key: DYN_API_KEY}
  - name: dyn.metadata
    summary: "Read backend metadata."
    duration: fast
    http: {verb: GET, path: /api/metadata}
`

// An app with backend.url_secret must generate a client that re-resolves the
// base URL per request (BaseURLFunc), a helper that reads it from secrets.json,
// and a broker-signup handler that caches the returned backend_url under the
// url_secret key — so calls after signup reach the user's own backend.
func TestURLSecretGeneratesDynamicBaseURL(t *testing.T) {
	cfg := parseSpec(t, urlSecretSpec)
	if cfg.Backend.URLSecret != "DYN_BACKEND_URL" {
		t.Fatalf("url_secret not parsed: %q", cfg.Backend.URLSecret)
	}
	dir := t.TempDir()
	written, err := Generate(cfg, dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, w := range written {
		if strings.HasSuffix(w, ".go") {
			if _, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, w), nil, parser.AllErrors); err != nil {
				t.Errorf("%s: not valid Go: %v", w, err)
			}
		}
	}

	main := readFile(t, dir, filepath.Join("cmd", "dyn-app", "main.go"))
	mustContain(t, "main.go", main,
		"BaseURLFunc:", "resolveBackendURLDyn", `"DYN_BACKEND_URL"`)

	client := readFile(t, dir, filepath.Join("internal", "backend", "client.go"))
	mustContain(t, "client.go", client, "baseURLFunc", "func (c *Client) base()")

	brokerSignup := readFile(t, dir, filepath.Join("cmd", "dyn-app", "broker_signup.go"))
	mustContain(t, "broker_signup.go", brokerSignup, "urlKey", "BackendURL", "backend_url")
}
