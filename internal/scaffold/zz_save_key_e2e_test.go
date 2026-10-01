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

// saveKeySpec mirrors Dial: signup is ordinary endpoints, and the key arrives
// once in the body of a verify call rather than from a signup route.
const saveKeySpec = `
id: io.pilot.savekeyx
app_version: 0.1.0
description: "App whose verify endpoint issues the API key."
namespace: savekeyx
backend:
  base_url: https://placeholder.invalid
  auth: byo
  headers:
    Authorization: "Bearer ${SAVEKEYX_API_KEY}"
methods:
  - name: savekeyx.signup
    summary: "Start signup."
    http: { verb: POST, path: "/auth/signup", public: true }
    params: { email: the email }
  - name: savekeyx.verify
    summary: "Finish signup; returns the key once."
    http: { verb: POST, path: "/auth/verify", public: true, save_key: { path: account.apiKey, secret_key: SAVEKEYX_API_KEY, start: savekeyx.signup } }
    params: { code: the code }
  - name: savekeyx.me
    summary: "Authenticated read."
    http: { verb: GET, path: "/me" }
`

func TestSaveKeyStoresRedactsAndSends(t *testing.T) {
	cfg := parseSpec(t, saveKeySpec)
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("spec invalid: %v", errs)
	}
	if !cfg.HasSignup() || !cfg.HasKeyMintSignup() || cfg.AuthSecretKey() != "SAVEKEYX_API_KEY" {
		t.Fatalf("save_key route should count as the key-minting route")
	}
	if got := cfg.SignupMethodName(); got != "savekeyx.signup" {
		t.Errorf("SignupMethodName()=%q, want the save_key start method", got)
	}
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/verify":
			_, _ = w.Write([]byte(`{"account":{"id":"acc_1","apiKey":"sk_live_secret"},"created":true}`))
		case "/me":
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"id":"acc_1"}`))
		default:
			_, _ = w.Write([]byte(`{"verificationId":"v_1"}`))
		}
	}))
	defer srv.Close()

	root := t.TempDir()
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
	sockDir, err := os.MkdirTemp("", "skx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "a.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	adapter.Env = append(os.Environ(), "SAVEKEYX_BACKEND_URL="+srv.URL)
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() }()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
	}
	call := func(method, args string) string {
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
		return string(out)
	}

	// Before any key: the authenticated method points at the signup start.
	if out := call("savekeyx.me", `{}`); !strings.Contains(out, `"activate":"savekeyx.signup"`) {
		t.Errorf("no-key call should point at savekeyx.signup: %s", out)
	}
	// Public signup works keyless.
	if out := call("savekeyx.signup", `{"email":"a@b.c"}`); !strings.Contains(out, "v_1") {
		t.Errorf("signup: %s", out)
	}
	// verify issues the key: it is stored, and the reply no longer carries it.
	out := call("savekeyx.verify", `{"code":"123456"}`)
	if strings.Contains(out, "sk_live_secret") {
		t.Fatalf("key leaked to the agent: %s", out)
	}
	if !strings.Contains(out, "saved on this host") || !strings.Contains(out, "acc_1") {
		t.Errorf("verify reply should keep the rest and mark the key saved: %s", out)
	}
	sec, _ := os.ReadFile(filepath.Join(proj, "secrets.json"))
	if !strings.Contains(string(sec), `"SAVEKEYX_API_KEY": "sk_live_secret"`) {
		t.Errorf("key not cached: %s", sec)
	}
	// Later calls carry it.
	call("savekeyx.me", `{}`)
	if gotAuth != "Bearer sk_live_secret" {
		t.Errorf("Authorization = %q, want the saved key", gotAuth)
	}
}

func TestSaveKeyValidation(t *testing.T) {
	for name, spec := range map[string]string{
		"missing secret_key": strings.Replace(saveKeySpec, ", secret_key: SAVEKEYX_API_KEY", "", 1),
		"unknown start":      strings.Replace(saveKeySpec, "start: savekeyx.signup", "start: savekeyx.nope", 1),
	} {
		cfg, err := Parse([]byte(spec))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		cfg.Resolve()
		if errs := cfg.Validate(); len(errs) == 0 {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
