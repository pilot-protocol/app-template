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

// cliAssetsSpec is a cli app that delivers its binary from the R2 artifact
// registry: an asset per host plus an enumerated + passthrough method. It
// exercises the generated staging runtime (backend/stage.go) and the asset-aware
// main, both of which only render when assets are present.
const cliAssetsSpec = `
id: io.pilot.toolx
app_version: 0.2.0
description: "Delivers and wraps the toolx CLI."
backend:
  type: cli
  command: ["toolx"]
assets:
  - {os: darwin, arch: arm64, url: "https://pub-x.r2.dev/io.pilot.toolx/0.2.0/darwin-arm64/toolx", sha256: "1111111111111111111111111111111111111111111111111111111111111111", exec_path: bin/toolx, order: 1}
  - {os: linux,  arch: amd64, url: "https://pub-x.r2.dev/io.pilot.toolx/0.2.0/linux-amd64/toolx",  sha256: "2222222222222222222222222222222222222222222222222222222222222222", exec_path: bin/toolx, order: 1}
methods:
  - name: toolx.version
    summary: "Print version."
    duration: fast
    cli: {args: ["version"]}
  - name: toolx.exec
    summary: "Passthrough."
    duration: med
    cli: {passthrough: true}
`

// TestGeneratedCLIWithAssetsCompiles type-checks the asset-delivery code paths:
// the staging runtime and the asset-aware main are generated only when an app
// ships assets, so an unused import or a bad template there is invisible to the
// no-asset cli compile test. It also asserts install.json is emitted.
func TestGeneratedCLIWithAssetsCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	cfg := parseSpec(t, cliAssetsSpec)
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "install.json")); err != nil {
		t.Fatalf("install.json must be emitted for an asset-delivering app: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "backend", "stage.go")); err != nil {
		t.Fatalf("stage.go must be generated for an asset-delivering app: %v", err)
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
		t.Fatalf("generated cli+assets project failed to compile: %v\n%s", err, out)
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
