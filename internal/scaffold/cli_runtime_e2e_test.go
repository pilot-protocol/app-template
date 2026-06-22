//go:build !windows

package scaffold

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// TestCLIAdapterRuntimeE2E is the end-to-end proof of "translate all CLI
// commands": it scaffolds a cli adapter fronting a real local script, builds it,
// runs it as the pilot daemon would (--socket/--manifest), and drives both
// method shapes over the actual app-store IPC protocol. An enumerated method maps
// to a fixed subcommand; a passthrough method forwards a verbatim argv array —
// i.e. `pilotctl appstore call <app> run {"args":[...]}` becomes `<cli> ...`.
func TestCLIAdapterRuntimeE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	root := t.TempDir()

	// A real local CLI: JSON for `status`, arg-reflection for `echo`, stderr +
	// non-zero exit for `fail`.
	tool := filepath.Join(root, "faketool")
	script := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  status) echo '{\"ok\":true,\"state\":\"green\"}';;\n" +
		"  echo) shift; echo \"got: $*\";;\n" +
		"  fail) echo boom >&2; exit 7;;\n" +
		"  *) echo \"unknown: $1\" >&2; exit 2;;\n" +
		"esac\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := `
id: io.pilot.faketool
app_version: 0.1.0
description: "Fronts faketool."
namespace: faketool
backend:
  type: cli
  command: ["` + tool + `"]
methods:
  - name: faketool.status
    summary: "Status."
    cli: {args: ["status"]}
  - name: faketool.run
    summary: "Passthrough."
    cli: {passthrough: true}
`
	cfg := parseSpec(t, spec)
	proj := filepath.Join(root, "proj")
	if _, err := Generate(cfg, proj); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Seed go.sum from the parent module so the build is hermetic/offline.
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

	sock := filepath.Join(root, "app.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() }()

	// Wait for the socket to appear.
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

	// Enumerated method → fixed subcommand, JSON passed through verbatim.
	if got := string(call("faketool.status", "{}")); got != `{"ok":true,"state":"green"}` {
		t.Errorf("status: got %s", got)
	}

	// Passthrough → arbitrary subcommand+args become the CLI argv.
	var echo struct {
		Stdout string `json:"stdout"`
		Exit   int    `json:"exit"`
	}
	if err := json.Unmarshal(call("faketool.run", `{"args":["echo","hello","world"]}`), &echo); err != nil {
		t.Fatalf("run echo: %v", err)
	}
	if echo.Stdout != "got: hello world" || echo.Exit != 0 {
		t.Errorf("passthrough echo: got %+v", echo)
	}

	// Passthrough non-zero exit is surfaced structurally (not an IPC error).
	var fail struct {
		Stderr string `json:"stderr"`
		Exit   int    `json:"exit"`
	}
	if err := json.Unmarshal(call("faketool.run", `{"args":["fail"]}`), &fail); err != nil {
		t.Fatalf("run fail: %v", err)
	}
	if fail.Exit != 7 || fail.Stderr != "boom" {
		t.Errorf("passthrough fail: got %+v", fail)
	}

	// Discovery method works locally with no backend call.
	if got := string(call("faketool.help", "{}")); !json.Valid([]byte(got)) || got == "" {
		t.Errorf("help: invalid: %s", got)
	}
}
