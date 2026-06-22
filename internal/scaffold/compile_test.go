package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// cliPassthroughSpec exercises both cli method shapes (enumerated + passthrough)
// and env_passthrough, so the compile test covers every generated cli code path.
const cliPassthroughSpec = `
id: io.pilot.toolx
app_version: 0.2.0
description: "Wraps the toolx CLI."
backend:
  type: cli
  command: ["toolx"]
  env_passthrough: [TOOLX_TOKEN]
methods:
  - name: toolx.run
    summary: "Run a named job."
    duration: med
    cli: {args: ["run", "--name", "${name}"]}
  - name: toolx.exec
    summary: "Passthrough: any toolx subcommand."
    duration: med
    cli: {passthrough: true}
`

// TestGeneratedCLIProjectCompiles is the load-bearing guard the parse-only
// TestGenerateProducesValidGo cannot provide: it actually type-checks the
// generated cli project with `go build ./...`. An unused variable (e.g. the
// http-only cfg leaking into the cli main) parses fine but fails to compile —
// exactly the regression this catches. go.sum entries are keyed by
// module@version, so reusing the parent module's checksums keeps the build
// hermetic and offline.
func TestGeneratedCLIProjectCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	cfg := parseSpec(t, cliPassthroughSpec)
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
		t.Fatalf("generated cli project failed to compile: %v\n%s", err, out)
	}
}

// TestCLIRouteValidation pins the cli route rules: passthrough is mutually
// exclusive with baked args/flags, and an empty route is rejected.
func TestCLIRouteValidation(t *testing.T) {
	cases := []struct {
		name      string
		route     string
		wantError bool
	}{
		{"baked args", `cli: {args: ["run"]}`, false},
		{"passthrough only", `cli: {passthrough: true}`, false},
		{"params as flags", `cli: {params_as_flags: true}`, false},
		{"passthrough with args", `cli: {passthrough: true, args: ["run"]}`, true},
		{"empty route", `cli: {}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := `
id: io.pilot.toolx
app_version: 0.1.0
description: "x"
backend: {type: cli, command: ["toolx"]}
methods:
  - name: toolx.m
    summary: "m"
    ` + tc.route + `
`
			cfg, err := Parse([]byte(spec))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg.Resolve()
			errs := cfg.Validate()
			if tc.wantError && len(errs) == 0 {
				t.Errorf("expected a validation error, got none")
			}
			if !tc.wantError && len(errs) != 0 {
				t.Errorf("expected no validation error, got %v", errs)
			}
		})
	}
}
