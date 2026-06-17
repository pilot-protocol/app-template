package scaffold

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseSpec parses, resolves, and validates a spec, failing on any error.
func parseSpec(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.Resolve()
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Fatalf("validate: %v", errs)
	}
	return cfg
}

const httpSpec = `
id: io.pilot.weather
app_version: 0.1.0
description: "Weather over the public corpus."
backend:
  type: http
  base_url: https://weather.example.com
methods:
  - name: weather.current
    summary: "Current conditions."
    duration: fast
    http: {verb: GET, path: /current}
    params: {lat: "string", lon: "string"}
  - name: weather.report
    summary: "Briefing."
    duration: slow
    http: {verb: POST, path: /report}
`

const cliSpec = `
id: io.pilot.toolx
app_version: 0.2.0
description: "Wraps the toolx CLI."
backend:
  type: cli
  command: ["toolx"]
methods:
  - name: toolx.run
    summary: "Run toolx."
    duration: med
    cli: {args: ["run", "--name", "${name}"]}
`

const authSpec = `
id: io.pilot.parallel
app_version: 0.1.0
description: "Search via an authenticated API."
backend:
  type: http
  base_url: https://api.parallel.ai
  headers:
    x-api-key: "${PARALLEL_API_KEY}"
methods:
  - name: parallel.search
    summary: "Search."
    duration: med
    http: {verb: POST, path: /v1/search}
listing:
  display_name: Parallel
  vendor: {name: Parallel}
  license: Apache-2.0
  categories: [search]
`

// TestGenerateProducesValidGo is the load-bearing test: every backend shape must
// generate Go that parses. (format.Source in Generate already rejects invalid
// Go, but parsing here pins the contract independently.)
func TestGenerateProducesValidGo(t *testing.T) {
	for _, tc := range []struct{ name, spec string }{
		{"http", httpSpec},
		{"cli", cliSpec},
		{"http-auth", authSpec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseSpec(t, tc.spec)
			dir := t.TempDir()
			written, err := Generate(cfg, dir)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			var sawMain, sawBackend bool
			for _, w := range written {
				if !strings.HasSuffix(w, ".go") {
					continue
				}
				if strings.HasSuffix(w, "main.go") {
					sawMain = true
				}
				if strings.Contains(w, "backend") {
					sawBackend = true
				}
				path := filepath.Join(dir, w)
				if _, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors); err != nil {
					t.Errorf("%s: not valid Go: %v", w, err)
				}
			}
			if !sawMain || !sawBackend {
				t.Errorf("expected a main.go and a backend file, got %v", written)
			}
		})
	}
}

func TestManifestExposesEveryMethodPlusHelp(t *testing.T) {
	cfg := parseSpec(t, httpSpec)
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatal(err)
	}
	mf, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"weather.current", "weather.report", "weather.help", "net.dial", "weather.example.com"} {
		if !strings.Contains(string(mf), want) {
			t.Errorf("manifest missing %q", want)
		}
	}
}

func TestValidateCatchesBadSpec(t *testing.T) {
	bad := `
id: not-reverse-dns
app_version: v1
description: ""
backend: {type: http}
methods:
  - name: wrongprefix.x
    http: {verb: PATCH, path: nope}
`
	cfg, err := Parse([]byte(bad))
	if err != nil {
		t.Fatalf("parse should succeed (semantic errors come from Validate): %v", err)
	}
	cfg.Resolve()
	errs := cfg.Validate()
	if len(errs) < 4 {
		t.Errorf("expected several validation errors, got %d: %v", len(errs), errs)
	}
}

func TestExampleSpecIsValid(t *testing.T) {
	parseSpec(t, ExampleSpec) // the shipped `pilot-app example` output must itself validate
}

func TestAuthAddsSecretsGrantAndHeaderWiring(t *testing.T) {
	cfg := parseSpec(t, authSpec)
	if !cfg.Backend.NeedsSecrets() {
		t.Fatal("expected NeedsSecrets() true for a ${...} header")
	}
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatal(err)
	}
	mf, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !strings.Contains(string(mf), "$APP/secrets.json") {
		t.Error("manifest should grant fs.read on $APP/secrets.json when headers use secrets")
	}
	if strings.Contains(string(mf), "127.0.0.1") {
		t.Error("manifest should NOT contain a stray 127.0.0.1 grant")
	}
	main, _ := os.ReadFile(filepath.Join(dir, "cmd", cfg.BinaryName, "main.go"))
	for _, want := range []string{"resolveHeaders", "expandSecrets", "x-api-key"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("generated main.go missing %q", want)
		}
	}
}

func TestBuildMetadataMirrorsSpec(t *testing.T) {
	cfg := parseSpec(t, authSpec)
	m := BuildMetadata(cfg)
	if m.SchemaVersion != 1 || m.ID != "io.pilot.parallel" || m.DisplayName != "Parallel" {
		t.Errorf("metadata core fields wrong: %+v", m)
	}
	if m.License != "Apache-2.0" || len(m.Categories) != 1 || m.Vendor.Name != "Parallel" {
		t.Errorf("listing fields not carried: %+v", m)
	}
	// methods = declared + auto help
	if len(m.Methods) != 2 || m.Methods[len(m.Methods)-1].Name != "parallel.help" {
		t.Errorf("expected declared methods + parallel.help, got %+v", m.Methods)
	}
	// runtime facts are filled later, not here
	if m.Size.BundleBytes != 0 || m.Vendor.PublisherPubkey != "" {
		t.Error("size/pubkey must be zero/empty until submit fills them")
	}
}

func TestNoSecretsGrantWhenNoSecretHeaders(t *testing.T) {
	cfg := parseSpec(t, httpSpec) // no headers
	if cfg.Backend.NeedsSecrets() {
		t.Fatal("plain http spec should not need secrets")
	}
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatal(err)
	}
	mf, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if strings.Contains(string(mf), "secrets.json") {
		t.Error("no secrets grant should appear without secret headers")
	}
}
