//go:build !windows

package scaffold

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// TestR2AssetDeliveryE2E is the full native-app proof: it scaffolds a cli
// adapter for a real, complex CLI whose binary is DELIVERED from the Pilot R2
// artifact registry (not assumed-installed), builds it, and runs it exactly as
// the daemon would. On first spawn the adapter reads install.json, fetches the
// tar.gz asset from its R2 URL, verifies the sha256, extracts it under $APP, and
// execs the staged command — proving discover→install→call works for a binary
// the host never had.
//
// Env-driven so the committed test needs no live bucket in CI; scripts/e2e-smolvm.sh
// uploads the artifact and sets these:
//
//	PILOT_E2E_ASSET_URL       https R2 public URL of the artifact (a .tar.gz)
//	PILOT_E2E_ASSET_SHA256    its sha256
//	PILOT_E2E_ASSET_EXECPATH  path to the command INSIDE the extracted tree
//	PILOT_E2E_ASSET_CALLARG   arg that makes the CLI print its version (e.g. --version)
//	PILOT_E2E_ASSET_EXPECT    substring the version output must contain (e.g. 1.2.0)
func TestR2AssetDeliveryE2E(t *testing.T) {
	url := os.Getenv("PILOT_E2E_ASSET_URL")
	sha := os.Getenv("PILOT_E2E_ASSET_SHA256")
	execPath := os.Getenv("PILOT_E2E_ASSET_EXECPATH")
	if url == "" || sha == "" || execPath == "" {
		t.Skip("set PILOT_E2E_ASSET_URL/_SHA256/_EXECPATH to run the live R2 delivery e2e (see scripts/e2e-smolvm.sh)")
	}
	callArg := envOr("PILOT_E2E_ASSET_CALLARG", "--version")
	expect := os.Getenv("PILOT_E2E_ASSET_EXPECT")
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	root := t.TempDir()
	// command basename must match the staged exec_path basename so the adapter
	// resolves the fronted command to the staged binary.
	cmd := filepath.Base(execPath)
	spec := fmt.Sprintf(`
id: io.pilot.smolvm
app_version: 1.2.0
description: "Delivers and fronts the smolvm microVM CLI from the R2 registry."
namespace: smolvm
backend:
  type: cli
  command: ["%s"]
assets:
  - os: %s
    arch: %s
    url: "%s"
    sha256: "%s"
    unpack: tar.gz
    exec_path: "%s"
    order: 1
methods:
  - name: smolvm.version
    summary: "Print the smolvm version."
    cli: {args: ["%s"]}
  - name: smolvm.exec
    summary: "Run any smolvm subcommand."
    cli: {passthrough: true}
`, cmd, runtime.GOOS, runtime.GOARCH, url, sha, execPath, callArg)

	cfg := parseSpec(t, spec)
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

	// Run the adapter as the daemon would. $APP is the manifest dir (proj), where
	// install.json was generated — the adapter stages the asset there on startup.
	sock := filepath.Join(root, "app.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() }()

	// Staging downloads + extracts the artifact BEFORE the socket appears, so
	// allow generous time for the fetch from R2.
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("adapter socket never appeared — staging from R2 likely failed (see adapter stderr above)")
	}

	// The asset must actually be on disk under $APP, delivered from R2.
	staged := filepath.Join(proj, filepath.FromSlash(execPath))
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged command not found at %s: %v", staged, err)
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

	// smolvm.version → the adapter execs the R2-delivered binary and returns its
	// output. Version text isn't JSON, so it comes back wrapped as {stdout,...}.
	var res struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Exit   int    `json:"exit"`
	}
	raw := call("smolvm.version", "{}")
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("smolvm.version reply not the wrapped shape: %v (raw=%s)", err, raw)
	}
	got := res.Stdout + res.Stderr
	t.Logf("smolvm.version via R2-delivered binary → exit=%d out=%q", res.Exit, got)
	if expect != "" && !contains(got, expect) {
		t.Fatalf("version output %q did not contain %q", got, expect)
	}

	// Discovery still works locally.
	if h := string(call("smolvm.help", "{}")); !json.Valid([]byte(h)) {
		t.Fatalf("smolvm.help invalid: %s", h)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
