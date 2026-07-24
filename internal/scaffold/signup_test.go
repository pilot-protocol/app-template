package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// signupSpec is a byo http app whose first method is a no-broker self-signup
// route: it mints a per-user key and caches it locally, and the other methods
// resolve that key from ${DIDIT_API_KEY} in their auth header. It exercises
// every generated signup code path plus the multi-host + secrets grants.
const signupSpec = `
id: io.pilot.didit
app_version: 1.0.0
description: "Identity verification over an HTTPS API with no-broker self-signup."
backend:
  type: http
  base_url: https://verification.didit.me/v3
  auth: byo
  headers:
    x-api-key: "${DIDIT_API_KEY}"
methods:
  - name: didit.signup
    summary: "Register a Didit account with your email (sends a one-time code)."
    duration: med
    signup:
      step: register
      url: https://apx.didit.me/auth/v2/programmatic/register/
  - name: didit.verify
    summary: "Submit the emailed code to mint + cache the API key."
    duration: med
    signup:
      step: verify
      url: https://apx.didit.me/auth/v2/programmatic/verify-email/
      secret_key: DIDIT_API_KEY
  - name: didit.billing_balance
    summary: "Check remaining credit balance."
    duration: fast
    http: {verb: GET, path: /billing/balance/}
`

// brokerSignupSpec is a byo http app whose signup is the fully-autonomous broker
// step (one call, no email) plus an account reader.
const brokerSignupSpec = `
id: io.pilot.didit
app_version: 1.0.0
description: "Identity verification with broker-side autonomous signup."
backend:
  type: http
  base_url: https://verification.didit.me/v3
  auth: byo
  headers:
    x-api-key: "${DIDIT_API_KEY}"
methods:
  - name: didit.signup
    summary: "Mint a Didit key via the broker — one call, no email."
    duration: slow
    signup:
      step: broker
      broker_url: https://broker.pilotprotocol.network/didit/signup
      secret_key: DIDIT_API_KEY
      email_key: DIDIT_ACCOUNT_EMAIL
  - name: didit.account
    summary: "Retrieve the cached account (email + key)."
    duration: fast
    signup:
      step: account
      secret_key: DIDIT_API_KEY
      email_key: DIDIT_ACCOUNT_EMAIL
  - name: didit.billing_balance
    summary: "Check remaining credit balance."
    duration: fast
    http: {verb: GET, path: /billing/balance/}
`

// createSignupSpec is a byo http app whose signup is the single-call emailless
// "create" step: one unauthenticated POST mints + caches the key (no email, no
// OTP, no broker). A second method is marked gated to exercise the free-plan
// disclaimer, and a plain method exercises the requireKey soft-fail wrap.
const createSignupSpec = `
id: io.pilot.primitive
app_version: 1.0.0
description: "Email infrastructure for AI agents with emailless self-signup."
backend:
  type: http
  base_url: https://api.primitive.dev/v1
  auth: byo
  headers:
    Authorization: "Bearer ${PRIMITIVE_API_KEY}"
methods:
  - name: primitive.signup
    summary: "Provision a free account + managed inbox in one call (no email)."
    duration: med
    signup:
      step: create
      url: https://api.primitive.dev/v1/agent/accounts
      secret_key: PRIMITIVE_API_KEY
      email_key: PRIMITIVE_ADDRESS
      body: {terms_accepted: true, device_name: "pilot appstore adapter"}
  - name: primitive.get_account
    summary: "Get account info."
    duration: fast
    http: {verb: GET, path: /account}
  - name: primitive.create_function
    summary: "Deploy a hosted JavaScript function."
    duration: slow
    gated: "requires the developer plan — confirm an email at primitive.dev to upgrade"
    http: {verb: POST, path: /functions}
`

func TestCreateSignupGrantsAndCompiles(t *testing.T) {
	cfg := parseSpec(t, createSignupSpec)
	if !cfg.HasSignup() || !cfg.HasKeyMintSignup() {
		t.Fatal("HasSignup() and HasKeyMintSignup() should be true")
	}
	if got := cfg.AuthSecretKey(); got != "PRIMITIVE_API_KEY" {
		t.Errorf("AuthSecretKey()=%q, want PRIMITIVE_API_KEY", got)
	}
	if got := cfg.SignupMethodName(); got != "primitive.signup" {
		t.Errorf("SignupMethodName()=%q, want primitive.signup", got)
	}
	// create-step defaults: key + address paths under the {success,data} envelope.
	var create *Method
	for i := range cfg.Methods {
		if cfg.Methods[i].Signup != nil && cfg.Methods[i].Signup.IsCreate() {
			create = &cfg.Methods[i]
		}
	}
	if create == nil {
		t.Fatal("expected a create signup method")
	}
	if create.Signup.KeyPath != "data.api_key" {
		t.Errorf("KeyPath default = %q, want data.api_key", create.Signup.KeyPath)
	}
	if create.Signup.AddressPath != "data.address" {
		t.Errorf("AddressPath default = %q, want data.address", create.Signup.AddressPath)
	}
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	mf, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	for _, want := range []string{
		`"cap": "fs.write", "target": "$APP/secrets.json"`,
		`"cap": "fs.read", "target": "$APP/secrets.json"`,
		`"target": "api.primitive.dev"`,
		`"primitive.signup"`,
	} {
		if !strings.Contains(string(mf), want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	main, _ := os.ReadFile(filepath.Join(dir, "cmd", cfg.BinaryName, "main.go"))
	for _, want := range []string{"signupCreateHandler", "requireKey(", "GatedNote", "NOT AVAILABLE ON THE FREE PLAN"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("generated main.go missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cmd", cfg.BinaryName, "signup.go")); err != nil {
		t.Errorf("expected generated signup.go: %v", err)
	}
	if testing.Short() {
		return
	}
	compileGenerated(t, dir)
}

func TestBrokerSignupGrantsAndCompiles(t *testing.T) {
	cfg := parseSpec(t, brokerSignupSpec)
	if !cfg.HasBrokerSignup() {
		t.Fatal("HasBrokerSignup() should be true")
	}
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	mf, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	for _, want := range []string{
		`"cap": "key.sign", "target": "self"`,
		`"cap": "fs.write", "target": "$APP/secrets.json"`,
		`"target": "broker.pilotprotocol.network"`,
		`"didit.signup"`, `"didit.account"`,
	} {
		if !strings.Contains(string(mf), want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	for _, f := range []string{"broker_signup.go", "signup.go"} {
		if _, err := os.Stat(filepath.Join(dir, "cmd", cfg.BinaryName, f)); err != nil {
			t.Errorf("expected generated %s: %v", f, err)
		}
	}
	// the signer.go (from the backend package) must be emitted for the broker call
	if _, err := os.Stat(filepath.Join(dir, "internal", "backend", "signer.go")); err != nil {
		t.Errorf("expected generated signer.go: %v", err)
	}
	if testing.Short() {
		return
	}
	compileGenerated(t, dir)
}

// compileGenerated seeds go.sum and `go build ./...` the generated project.
func compileGenerated(t *testing.T, dir string) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
	}
	cmd := exec.Command(goBin, "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated broker-signup project failed to compile: %v\n%s", err, out)
	}
}

func TestSignupConfigResolvesDefaults(t *testing.T) {
	cfg := parseSpec(t, signupSpec)
	if !cfg.HasSignup() {
		t.Fatal("HasSignup() should be true")
	}
	var reg, ver *Method
	for i := range cfg.Methods {
		switch {
		case cfg.Methods[i].Signup != nil && cfg.Methods[i].Signup.IsVerify():
			ver = &cfg.Methods[i]
		case cfg.Methods[i].Signup != nil:
			reg = &cfg.Methods[i]
		}
	}
	if reg == nil || ver == nil {
		t.Fatal("expected a register + a verify signup method")
	}
	if reg.Signup.Step != "register" || ver.Signup.Step != "verify" {
		t.Errorf("steps = %q / %q", reg.Signup.Step, ver.Signup.Step)
	}
	if ver.Signup.KeyPath != "application.api_key" {
		t.Errorf("KeyPath default = %q, want application.api_key", ver.Signup.KeyPath)
	}
	if reg.Signup.EmailKey != "ACCOUNT_EMAIL" || reg.Signup.PasswordKey != "ACCOUNT_PASSWORD" {
		t.Errorf("email/password key defaults = %q/%q", reg.Signup.EmailKey, reg.Signup.PasswordKey)
	}
	// The signup auth host is the extra dial target (base excluded).
	hosts := strings.Join(cfg.SignupHosts(), ",")
	if !strings.Contains(hosts, "apx.didit.me") {
		t.Errorf("SignupHosts()=%q, want apx.didit.me", hosts)
	}
	if strings.Contains(hosts, "verification.didit.me") {
		t.Errorf("SignupHosts() must exclude the base_url host, got %q", hosts)
	}
}

func TestSignupValidationRejectsBadRoutes(t *testing.T) {
	cases := map[string]string{
		"verify missing secret_key": `
id: io.pilot.x
app_version: 1.0.0
description: d
backend: {type: http, base_url: https://x.example.com, auth: byo, headers: {x-api-key: "${X}"}}
methods:
  - {name: x.verify, summary: s, duration: med, signup: {step: verify, url: https://a.example.com/v}}
`,
		"bad step": `
id: io.pilot.x
app_version: 1.0.0
description: d
backend: {type: http, base_url: https://x.example.com, auth: byo, headers: {x-api-key: "${X}"}}
methods:
  - {name: x.signup, summary: s, duration: med, signup: {step: bogus, url: https://a.example.com/r}}
`,
		"http-scheme url": `
id: io.pilot.x
app_version: 1.0.0
description: d
backend: {type: http, base_url: https://x.example.com, auth: byo, headers: {x-api-key: "${X}"}}
methods:
  - {name: x.signup, summary: s, duration: med, signup: {step: register, url: http://a.example.com/r}}
`,
		"signup+http both": `
id: io.pilot.x
app_version: 1.0.0
description: d
backend: {type: http, base_url: https://x.example.com, auth: byo, headers: {x-api-key: "${X}"}}
methods:
  - {name: x.signup, summary: s, duration: med, http: {verb: GET, path: /a}, signup: {step: register, url: https://a.example.com/r}}
`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(spec))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg.Resolve()
			if errs := cfg.Validate(); len(errs) == 0 {
				t.Fatalf("expected a validation error for %q", name)
			}
		})
	}
}

func TestSignupManifestGrants(t *testing.T) {
	cfg := parseSpec(t, signupSpec)
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	mf, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	s := string(mf)
	for _, want := range []string{
		`"cap": "fs.write", "target": "$APP/secrets.json"`,
		`"cap": "fs.read", "target": "$APP/secrets.json"`,
		`"target": "apx.didit.me"`,
		`"target": "verification.didit.me"`,
		`"didit.signup"`,
		`"didit.verify"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest missing %q\n%s", want, s)
		}
	}
	// The generated signup runtime lands next to main.go.
	if _, err := os.Stat(filepath.Join(dir, "cmd", cfg.BinaryName, "signup.go")); err != nil {
		t.Errorf("expected generated signup.go: %v", err)
	}
}

// TestGeneratedSignupProjectCompiles type-checks the generated signup adapter
// with `go build ./...` — the load-bearing guard that the signup.go template
// and its main.go wiring produce valid, compiling Go.
func TestGeneratedSignupProjectCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	cfg := parseSpec(t, signupSpec)
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("seed go.sum: %v", err)
		}
	}
	cmd := exec.Command(goBin, "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated signup project failed to compile: %v\n%s", err, out)
	}
}
