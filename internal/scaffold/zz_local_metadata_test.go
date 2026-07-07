package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localMetaSpec is a managed HTTP app that captures a method's response to a
// host-local metadata file and exposes a local read method over the same file.
const localMetaSpec = `
id: io.pilot.locx
app_version: 0.1.0
description: "Local metadata capture + read."
backend:
  base_url: https://api.example.com
  auth: managed
methods:
  - name: locx.buy
    summary: "Buy a thing; captures the result to a local store."
    http: { verb: POST, path: /v1/things, capture_to: "~/.pilot/.locx" }
  - name: locx.mine
    summary: "Recall the things this host bought (local, no backend call)."
    local: { store: "~/.pilot/.locx" }
`

// TestGeneratedLocalMetadataCompiles verifies the local-metadata feature wires
// correctly: a local read method + a capture_to on an http method, the fs grants
// for the store, and that the whole project type-checks.
func TestGeneratedLocalMetadataCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	cfg := parseSpec(t, localMetaSpec)
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
		`localRead(expandHome("~/.pilot/.locx"))`,           // local method reads the store
		`captureWrap(expandHome("~/.pilot/.locx"), forward`, // http method captures into it
		`func appendEntry(`, `func localRead(`,              // helpers generated
	} {
		if !strings.Contains(string(main), want) {
			t.Errorf("generated main.go missing: %s", want)
		}
	}

	// The manifest must grant fs.read + fs.write on the store ($HOME-normalized).
	mf, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{
		`{"cap": "fs.read", "target": "$HOME/.pilot/.locx"}`,
		`{"cap": "fs.write", "target": "$HOME/.pilot/.locx"}`,
	} {
		if !strings.Contains(string(mf), want) {
			t.Errorf("manifest missing grant: %s", want)
		}
	}

	cmd := exec.Command(goBin, "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated local-metadata project failed to compile: %v\n%s", err, out)
	}
}

// TestLocalMethodValidation pins the local-route rules.
func TestLocalMethodValidation(t *testing.T) {
	base := `
id: io.pilot.locx
app_version: 0.1.0
description: "x"
backend: { base_url: https://api.example.com, auth: managed }
methods:
`
	cases := []struct {
		name    string
		method  string
		wantErr bool
	}{
		{"valid local", "  - {name: locx.mine, summary: m, local: {store: \"~/.pilot/.locx\"}}", false},
		{"local no store", "  - {name: locx.mine, summary: m, local: {store: \"\"}}", true},
		{"local + http", "  - {name: locx.mine, summary: m, local: {store: \"~/x\"}, http: {verb: GET, path: /x}}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := parseResolved(t, base+tc.method+"\n")
			errs := c.Validate()
			if tc.wantErr && len(errs) == 0 {
				t.Error("expected a validation error, got none")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}
