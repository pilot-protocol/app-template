//go:build !windows

package scaffold

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// resolveSpec is a byo http app exercising every per-param `in` location plus
// the non-JSON response path:
//
//   - fetch   GET /{url}      url in:path_raw  → raw, UNescaped URL in the path
//   - search  GET /search     q in:query, key in:header → query + request header
//   - submit  POST /submit    payload in:body, trace in:header → body field + header
//   - text    GET /text       returns text/markdown → wrapped into {content_type,content}
const resolveSpec = `
id: io.pilot.resolvex
app_version: 0.1.0
description: "App exercising every param location."
namespace: resolvex
backend:
  base_url: https://placeholder.invalid
methods:
  - name: resolvex.fetch
    summary: "URL-in-path (raw)."
    http: { verb: GET, path: "/{url}", param_in: { url: path_raw } }
    params: { url: target url }
  - name: resolvex.search
    summary: "query + header."
    http: { verb: GET, path: "/search", param_in: { q: query, key: header } }
    params: { q: the query, key: api key }
  - name: resolvex.submit
    summary: "body + header."
    http: { verb: POST, path: "/submit", param_in: { payload: body, trace: header } }
    params: { payload: the payload, trace: trace id }
  - name: resolvex.text
    summary: "non-JSON response."
    http: { verb: GET, path: "/text" }
`

// TestGeneratedHTTPParamResolutionE2E is the end-to-end proof of per-param
// resolution: it scaffolds a byo http adapter, points it (via the *_BACKEND_URL
// override) at a real httptest backend, builds + runs it as the daemon would,
// and drives each `in` location over the actual IPC protocol — asserting the
// backend saw the raw (unescaped) path, the query value, the header, the body
// field, and that a text/markdown 2xx body comes back wrapped as
// {content_type, content}.
func TestGeneratedHTTPParamResolutionE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	// Record what the backend actually received.
	var gotPath, gotRawQuery, gotKeyHeader, gotTraceHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			gotRawQuery = r.URL.RawQuery
			gotKeyHeader = r.Header.Get("key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/submit":
			gotTraceHeader = r.Header.Get("trace")
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			gotBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/text":
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte("# Hello\n\nplain markdown, not JSON"))
		default:
			// fetch: the raw path-param URL lands here verbatim. r.URL.Path is
			// already %-decoded by net/http, so compare against RequestURI to see
			// whether the client escaped it on the way out.
			gotPath = r.RequestURI
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	cfg := parseSpec(t, resolveSpec)
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

	// The unix socket path must stay under the OS sun_path limit (~104 on
	// darwin), so keep it in a short-named dir rather than the long test temp.
	sockDir, err := os.MkdirTemp("", "rxsk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "a.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	// Point the adapter at the test backend via the generated env override.
	adapter.Env = append(os.Environ(), "RESOLVEX_BACKEND_URL="+srv.URL)
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	call := func(method, args string) json.RawMessage {
		t.Helper()
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
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

	// path_raw: the URL must reach the backend UNescaped — a real https://host
	// in the path, not https:%2F%2F. This is the behavior plainweb depends on.
	call("resolvex.fetch", `{"url":"https://example.com/a?b=c"}`)
	if !strings.HasPrefix(gotPath, "/https://example.com/a") {
		t.Errorf("path_raw escaped or mangled: backend saw RequestURI %q, want a raw /https://example.com/... prefix", gotPath)
	}

	// query + header.
	call("resolvex.search", `{"q":"hello world","key":"sekret"}`)
	if gotRawQuery != "q=hello+world" && gotRawQuery != "q=hello%20world" {
		t.Errorf("query not encoded: backend saw RawQuery %q", gotRawQuery)
	}
	if gotKeyHeader != "sekret" {
		t.Errorf("header param not sent: backend saw key header %q", gotKeyHeader)
	}

	// body field + header on a POST.
	call("resolvex.submit", `{"payload":{"n":1},"trace":"t-123"}`)
	if !strings.Contains(gotBody, `"payload"`) || strings.Contains(gotBody, `"trace"`) {
		t.Errorf("body field misrouted: backend saw body %q (trace must NOT be in the body)", gotBody)
	}
	if gotTraceHeader != "t-123" {
		t.Errorf("header param not sent on POST: backend saw trace header %q", gotTraceHeader)
	}

	// Non-JSON 2xx body wraps into {content_type, content}.
	var wrapped struct {
		ContentType string `json:"content_type"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(call("resolvex.text", "{}"), &wrapped); err != nil {
		t.Fatalf("text response not valid JSON: %v", err)
	}
	if !strings.HasPrefix(wrapped.ContentType, "text/markdown") {
		t.Errorf("wrapped content_type = %q, want text/markdown…", wrapped.ContentType)
	}
	if !strings.Contains(wrapped.Content, "plain markdown, not JSON") {
		t.Errorf("wrapped content = %q, want the markdown body", wrapped.Content)
	}
}
