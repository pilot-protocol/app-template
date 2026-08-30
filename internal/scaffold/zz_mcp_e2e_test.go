//go:build !windows

package scaffold

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// An app can front a credentialed REST API and an open tool server at the same
// time. The property that matters is that those two backends never share a
// credential: the REST key belongs to one origin, and shipping it to another
// because the same vendor happens to run both is how a key ends up somewhere
// its owner never agreed to.
//
// The generated ToolClient has no field to hold a credential, which makes the
// property structural rather than a habit — but structure is easy to erode, so
// this asserts it from the outside, against a real running adapter.
const mcpSpec = `
id: io.pilot.toolx
app_version: 0.1.0
description: "App with a credentialed REST backend and an open tool server."
namespace: toolx
backend:
  base_url: https://placeholder.invalid
  headers:
    authorization: "Bearer ${TOOLX_API_KEY}"
  mcp:
    url: https://placeholder.invalid/mcp/
methods:
  - name: toolx.rest_read
    summary: "A REST call that DOES carry the key."
    http: { verb: GET, path: /things }
  - name: toolx.options
    summary: "A tool call that must carry nothing."
    mcp: { tool: get_filing_options }
    params: { entity_type: "llc or c-corp" }
  - name: toolx.start
    summary: "A tool call taking one wrapped object argument."
    duration: slow
    mcp: { tool: start_llc_formation, wrap: intake }
    params: { company_name: "the name" }
  - name: toolx.update
    summary: "A tool call mixing a top-level id with a nested object."
    mcp: { tool: update_formation, wrap: update, keep: [formation_id] }
    params: { formation_id: "the id", company_name: "the new name" }
`

// toolServer records every header it is sent and answers the JSON-RPC calls the
// generated client makes. It replies over SSE, the harder of the two transports.
type toolServer struct {
	mu       sync.Mutex
	headers  []http.Header
	lastArgs map[string]json.RawMessage
	lastTool string
	calls    int
}

func newToolServer(t *testing.T) (*httptest.Server, *toolServer) {
	t.Helper()
	rec := &toolServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.headers = append(rec.headers, r.Header.Clone())
		rec.mu.Unlock()

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string                     `json:"name"`
				Arguments map[string]json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"protocolVersion\":\"2025-06-18\",\"serverInfo\":{\"name\":\"mock\"}}}\n\n", req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			rec.mu.Lock()
			rec.lastTool, rec.lastArgs, rec.calls = req.Params.Name, req.Params.Arguments, rec.calls+1
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			// structuredContent is the preferred shape; return real-looking data.
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"structuredContent\":{\"tool\":%q,\"ok\":true}}}\n\n",
				req.ID, req.Params.Name)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return srv, rec
}

func TestGeneratedToolServerNeverSeesTheRestKeyE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	tools, rec := newToolServer(t)
	defer tools.Close()

	restHit := make(chan http.Header, 4)
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restHit <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer rest.Close()

	root := t.TempDir()
	cfg := parseSpec(t, strings.Replace(mcpSpec, "https://placeholder.invalid/mcp/", tools.URL+"/mcp/", 1))
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("spec invalid: %v", errs)
	}
	proj := filepath.Join(root, "proj")
	if _, err := Generate(cfg, proj); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		_ = os.WriteFile(filepath.Join(proj, "go.sum"), sum, 0o644)
	}
	bin := filepath.Join(root, "adapter")
	build := exec.Command("go", "build", "-o", bin, "./cmd/"+cfg.BinaryName)
	build.Dir = proj
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v\n%s", err, out)
	}

	sockDir, err := os.MkdirTemp("", "tlsk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "a.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	// A real, secret-looking key on the REST side. It must never leave that origin.
	adapter.Env = append(os.Environ(),
		"TOOLX_BACKEND_URL="+rest.URL,
		"TOOLX_API_KEY=glk_super_secret_do_not_leak")
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	call := func(method, args string) json.RawMessage {
		t.Helper()
		conn, err := net.DialTimeout("unix", sock, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		var out json.RawMessage
		if err := ipc.Call(conn, method, json.RawMessage(args), &out); err != nil {
			t.Fatalf("call %s: %v", method, err)
		}
		return out
	}

	// The REST side really does carry the key — otherwise the negative below
	// would pass for the wrong reason.
	call("toolx.rest_read", `{}`)
	select {
	case h := <-restHit:
		if h.Get("Authorization") != "Bearer glk_super_secret_do_not_leak" {
			t.Fatalf("REST backend did not receive the key (got %q); the leak check below would be vacuous", h.Get("Authorization"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("REST backend was never called")
	}

	// Now exercise every tool route.
	out := call("toolx.options", `{"entity_type":"llc"}`)
	if !strings.Contains(string(out), `"ok":true`) {
		t.Errorf("tool result not unwrapped from structuredContent: %s", out)
	}
	call("toolx.start", `{"company_name":"NewCo"}`)
	call("toolx.update", `{"formation_id":"f-1","company_name":"Renamed"}`)

	// THE ASSERTION: nothing credential-shaped ever reached the tool server.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.headers) < 3 {
		t.Fatalf("tool server saw %d requests, expected the handshake plus three calls", len(rec.headers))
	}
	for i, h := range rec.headers {
		for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization", "Cookie"} {
			if v := h.Get(name); v != "" {
				t.Errorf("request %d carried %s: %q — the REST key must never reach the tool server", i, name, v)
			}
		}
		for name, vals := range h {
			for _, v := range vals {
				if strings.Contains(v, "glk_super_secret") {
					t.Errorf("request %d leaked the REST key in header %s: %q", i, name, v)
				}
			}
		}
	}

	// The session is established once and reused, not renegotiated per call.
	inits := 0
	for _, h := range rec.headers {
		if h.Get("Mcp-Session-Id") == "" {
			inits++
		}
	}
	if inits > 2 { // initialize + the initialized notification
		t.Errorf("tool server saw %d sessionless requests; the session should be established once and reused", inits)
	}

	// wrap/keep must shape the arguments the way the tool declares them.
	if rec.lastTool != "update_formation" {
		t.Fatalf("last tool = %q", rec.lastTool)
	}
	if got := string(rec.lastArgs["formation_id"]); got != `"f-1"` {
		t.Errorf("formation_id should stay top-level, got %q", got)
	}
	upd, ok := rec.lastArgs["update"]
	if !ok {
		t.Fatal("the wrapped `update` argument is missing")
	}
	if !strings.Contains(string(upd), "Renamed") {
		t.Errorf("update = %s, want it to carry company_name", upd)
	}
	if strings.Contains(string(upd), "formation_id") {
		t.Errorf("update = %s, want formation_id kept OUT of the wrapped object", upd)
	}
}

// TestMCPValidationRules: a spec that would generate a broken or credential-
// leaking tool client must fail at build time.
func TestMCPValidationRules(t *testing.T) {
	base := `
id: io.pilot.mcpv
app_version: 0.1.0
description: "fixture"
namespace: mcpv
backend:
  base_url: https://placeholder.invalid
%s
methods:
  - name: mcpv.t
    summary: "tool"
%s
`
	cases := []struct{ name, backend, method, want string }{
		{"tool is required",
			"  mcp:\n    url: https://example.com/mcp/", "    mcp: { tool: \"\" }", "mcp.tool is required"},
		{"backend.mcp.url must be set",
			"", "    mcp: { tool: x }", "backend.mcp.url is not set"},
		{"url must be https",
			"  mcp:\n    url: http://example.com/mcp/", "    mcp: { tool: x }", "must be an https URL"},
		{"no credentialed tool servers yet",
			"  mcp:\n    url: https://example.com/mcp/\n    auth: bearer", "    mcp: { tool: x }", "not supported"},
		{"keep needs wrap",
			"  mcp:\n    url: https://example.com/mcp/", "    mcp: { tool: x, keep: [a] }", "only means something with mcp.wrap"},
		{"cannot mix routes",
			"  mcp:\n    url: https://example.com/mcp/", "    mcp: { tool: x }\n    http: { verb: GET, path: /y }", "must not also declare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateSpec(t, fmt.Sprintf(base, tc.backend, tc.method))
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want an error mentioning %q, got: %s", tc.want, got)
			}
		})
	}
}

// TestMCPGrantsAndFiles: the tool server's host needs its own net.dial grant,
// and the client is only emitted for an app that actually has a tool route.
func TestMCPGrantsAndFiles(t *testing.T) {
	c := parseSpec(t, strings.Replace(mcpSpec, "https://placeholder.invalid/mcp/", "https://tools.example.com/mcp/", 1))
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatalf("spec invalid: %v", errs)
	}
	dir := t.TempDir()
	if _, err := Generate(c, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	man, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !strings.Contains(string(man), `"net.dial", "target": "tools.example.com"`) {
		t.Errorf("manifest is missing the tool-server net.dial grant:\n%s", man)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "backend", "mcp.go")); err != nil {
		t.Errorf("mcp.go not emitted: %v", err)
	}

	plain, _ := Parse([]byte(`
id: io.pilot.notools
app_version: 0.1.0
description: "no tools"
namespace: notools
backend:
  base_url: https://placeholder.invalid
methods:
  - name: notools.ping
    summary: "ping"
    http: { verb: GET, path: /ping }
`))
	plain.Resolve()
	d2 := t.TempDir()
	if _, err := Generate(plain, d2); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d2, "internal", "backend", "mcp.go")); err == nil {
		t.Error("mcp.go emitted for an app with no tool routes")
	}
}
