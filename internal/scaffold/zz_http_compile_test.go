package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// restPathParamSpec is a managed HTTP app exercising every generated HTTP shape:
// GET (list + path-param), POST body, PATCH body+path-param, DELETE path-param,
// and a multi-placeholder path. It compiles only if the new generic forward,
// the client's Do, and the conditional net/url + strings imports are all correct.
const restPathParamSpec = `
id: io.pilot.restx
app_version: 0.1.0
description: "REST app exercising path params and all verbs."
backend:
  base_url: https://api.example.com
  auth: managed
methods:
  - name: restx.list
    summary: "list things"
    http: { verb: GET, path: /v1/things }
  - name: restx.get
    summary: "get one"
    http: { verb: GET, path: "/v1/things/{id}" }
    params: { id: the thing id }
  - name: restx.create
    summary: "create"
    http: { verb: POST, path: /v1/things }
  - name: restx.update
    summary: "update"
    http: { verb: PATCH, path: "/v1/things/{id}" }
    params: { id: the thing id }
  - name: restx.remove
    summary: "delete"
    http: { verb: DELETE, path: "/v1/things/{id}" }
    params: { id: the thing id }
  - name: restx.nested
    summary: "nested path params"
    http: { verb: GET, path: "/v1/things/{id}/items/{item_id}" }
    params: { id: thing id, item_id: item id }
`

// TestGeneratedHTTPPathParamProjectCompiles type-checks a generated managed-HTTP
// adapter that uses path params and all five verbs. This is the load-bearing
// guard for the HTTP-adapter improvements: a template typo (bad import gating,
// wrong forward signature) parses fine but fails `go build`.
func TestGeneratedHTTPPathParamProjectCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	cfg := parseSpec(t, restPathParamSpec)
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("seed go.sum: %v", err)
		}
	}

	// The generated dispatcher must wire path params through the generic forward.
	mainSrc, err := os.ReadFile(filepath.Join(dir, "cmd", cfg.BinaryName, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, want := range []string{
		`method: "GET", pathTmpl: "/v1/things/{id}"`,
		`method: "PATCH", pathTmpl: "/v1/things/{id}"`,
		`method: "DELETE", pathTmpl: "/v1/things/{id}"`,
		`method: "GET", pathTmpl: "/v1/things/{id}/items/{item_id}"`,
	} {
		if !strings.Contains(string(mainSrc), want) {
			t.Errorf("generated main.go missing dispatcher wiring: %s", want)
		}
	}

	cmd := exec.Command(goBin, "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated http project failed to compile: %v\n%s", err, out)
	}
}
