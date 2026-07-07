package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const managedBalanceSpec = `
id: io.pilot.paidapi
app_version: 0.1.0
description: "A managed, credit-metered API."
backend:
  base_url: https://api.example.com
  auth: managed
methods:
  - name: paidapi.run
    summary: "Do a paid thing."
    http: { verb: POST, path: /v1/run }
`

const byoBalanceSpec = `
id: io.pilot.freeapi
app_version: 0.1.0
description: "A bring-your-own-key API (no broker, no budget)."
backend:
  base_url: https://api.example.com
  auth: byo
methods:
  - name: freeapi.run
    summary: "Do a thing."
    http: { verb: GET, path: /v1/run }
`

// hasMethod reports whether the resolved config carries a method by name.
func hasMethod(c *Config, name string) *Method {
	for i := range c.Methods {
		if c.Methods[i].Name == name {
			return &c.Methods[i]
		}
	}
	return nil
}

// TestBalanceMethod_InjectedForManagedOnly pins the auto-injected balance method:
// managed (broker + budget) apps get a free `<ns>.balance` wired to the reserved
// credit route; byo apps (no broker) don't.
func TestBalanceMethod_InjectedForManagedOnly(t *testing.T) {
	managed := parseSpec(t, managedBalanceSpec)
	bal := hasMethod(managed, "paidapi.balance")
	if bal == nil {
		t.Fatal("managed app missing auto-injected paidapi.balance method")
	}
	if bal.HTTP == nil || bal.HTTP.Verb != "GET" || bal.HTTP.Path != BalanceMetaPath {
		t.Fatalf("balance route = %+v, want GET %s", bal.HTTP, BalanceMetaPath)
	}
	if bal.Kind != "meta" {
		t.Errorf("balance Kind = %q, want meta", bal.Kind)
	}

	byo := parseSpec(t, byoBalanceSpec)
	if hasMethod(byo, "freeapi.balance") != nil {
		t.Error("byo app should NOT get a balance method (no broker/budget)")
	}
}

// TestBalanceMethod_Generated verifies the injected method reaches the generated
// adapter: registered against the reserved path, listed in the manifest exposes,
// discoverable in <ns>.help, and the project still compiles.
func TestBalanceMethod_Generated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	cfg := parseSpec(t, managedBalanceSpec)
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("validate: %v", errs)
	}
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
	}

	main, err := os.ReadFile(filepath.Join(dir, "cmd", cfg.BinaryName, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, want := range []string{
		`d.Register("paidapi.balance"`,        // registered
		`pathTmpl: "` + BalanceMetaPath + `"`, // dialing the reserved route
	} {
		if !strings.Contains(string(main), want) {
			t.Errorf("generated main.go missing: %s", want)
		}
	}

	mf, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(mf), `"paidapi.balance"`) {
		t.Error("manifest exposes list missing paidapi.balance")
	}

	cmd := exec.Command(goBin, "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated managed project failed to compile: %v\n%s", err, out)
	}
}
