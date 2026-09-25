package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setKeySpec is a plain byo app: the user creates a key at the provider and
// hands it to <ns>.set_key; every other method resolves it from secrets.json.
const setKeySpec = `
id: io.pilot.rentahuman
app_version: 0.2.1
description: "Hire humans for real-world tasks with your own RentAHuman API key."
backend:
  type: http
  base_url: https://rentahuman.ai
  auth: byo
  headers:
    X-API-Key: "${RENTAHUMAN_API_KEY}"
methods:
  - name: rentahuman.set_key
    summary: "Save the RentAHuman API key this host uses."
    duration: fast
    signup:
      step: set_key
      url: https://rentahuman.ai/account/api-keys
      secret_key: RENTAHUMAN_API_KEY
  - name: rentahuman.list_bounties
    summary: "List open bounties."
    duration: fast
    http: {verb: GET, path: /api/partner/v1/bounties}
  - name: rentahuman.list_humans
    summary: "Browse humans (public)."
    duration: fast
    http: {verb: GET, path: /api/humans, public: true}
`

func TestSetKeyStepGeneratesAndCompiles(t *testing.T) {
	cfg := parseSpec(t, setKeySpec)
	if !cfg.HasSignup() || !cfg.HasKeyMintSignup() {
		t.Fatal("HasSignup() and HasKeyMintSignup() should be true for a set_key step")
	}
	if got := cfg.AuthSecretKey(); got != "RENTAHUMAN_API_KEY" {
		t.Errorf("AuthSecretKey()=%q, want RENTAHUMAN_API_KEY", got)
	}
	if got := cfg.SignupMethodName(); got != "rentahuman.set_key" {
		t.Errorf("SignupMethodName()=%q, want rentahuman.set_key", got)
	}
	hint := cfg.SignupHint()
	for _, want := range []string{"rentahuman.set_key", "https://rentahuman.ai/account/api-keys", `"api_key"`} {
		if !strings.Contains(hint, want) {
			t.Errorf("SignupHint() %q missing %q", hint, want)
		}
	}
	if strings.Contains(hint, "no arguments required") {
		t.Errorf("set_key hint must not claim no arguments are needed: %q", hint)
	}

	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	main, _ := os.ReadFile(filepath.Join(dir, "cmd", cfg.BinaryName, "main.go"))
	for _, want := range []string{"setKeyHandler(setKeyConfig{", "requireKey(", "rentahuman.ai/account/api-keys", "HeaderFunc:"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("generated main.go missing %q", want)
		}
	}
	// The gated route is wrapped; the public one is not.
	if !strings.Contains(string(main), `requireKey(manifestPath, "RENTAHUMAN_API_KEY", "rentahuman.set_key", `) {
		t.Error("list_bounties should be wrapped in requireKey")
	}
	for _, line := range strings.Split(string(main), "\n") {
		if strings.Contains(line, `d.Register("rentahuman.list_humans"`) && strings.Contains(line, "requireKey(") {
			t.Errorf("public route must not be gated on a key: %s", line)
		}
	}
	mf, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !strings.Contains(string(mf), `"cap": "fs.write", "target": "$APP/secrets.json"`) {
		t.Errorf("manifest must grant fs.write on secrets.json for set_key")
	}
	if testing.Short() {
		return
	}
	compileGenerated(t, dir)
}

func TestSetKeyStepValidation(t *testing.T) {
	validate := func(spec string) []error {
		t.Helper()
		cfg, err := Parse([]byte(spec))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.Resolve()
		return cfg.Validate()
	}
	if errs := validate(setKeySpec); len(errs) != 0 {
		t.Fatalf("valid set_key spec rejected: %v", errs)
	}
	bad := strings.Replace(setKeySpec, "      secret_key: RENTAHUMAN_API_KEY\n", "", 1)
	errs := validate(bad)
	found := false
	for _, e := range errs {
		found = found || strings.Contains(e.Error(), "set_key step needs signup.secret_key")
	}
	if !found {
		t.Errorf("missing secret_key: errs=%v, want the set_key secret_key error", errs)
	}
	insecure := strings.Replace(setKeySpec, "https://rentahuman.ai/account/api-keys", "http://rentahuman.ai/account/api-keys", 1)
	if errs := validate(insecure); len(errs) == 0 {
		t.Error("an http:// set_key url should be rejected")
	}
}

// The register flow's hint must not tell agents to call it with no arguments.
func TestSignupHintRegisterStep(t *testing.T) {
	cfg := parseSpec(t, signupSpec)
	if h := cfg.SignupHint(); strings.Contains(h, "no arguments required") || !strings.Contains(h, "didit.signup") {
		t.Errorf("register hint = %q", h)
	}
}
