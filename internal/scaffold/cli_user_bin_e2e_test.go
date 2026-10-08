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

// A CLI the user installed in ~/.local/bin is found although the adapter's
// PATH lacks that directory. The daemon runs under launchd or systemd with
// PATH=/usr/bin:/bin:/usr/sbin:/sbin, so a bare command name that works in
// the user's shell failed in the app with "executable file not found".
func TestCLIAdapterFindsCommandInUserBinDir(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	userBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(userBin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"user-bin: $*\"\n"
	if err := os.WriteFile(filepath.Join(userBin, "userbintool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := parseSpec(t, `
id: io.pilot.userbintool
app_version: 0.1.0
description: "Fronts a CLI installed in ~/.local/bin."
namespace: userbintool
backend:
  type: cli
  command: ["userbintool"]
methods:
  - name: userbintool.run
    summary: "Passthrough."
    cli: {passthrough: true}
`)
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

	// Unix socket paths are capped at 104 bytes on macOS; this test's TempDir
	// name is too long for one.
	sockDir, err := os.MkdirTemp("", "ub")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "app.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	// The environment a launchd-started daemon hands its apps.
	adapter.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + os.TempDir()}
	adapter.Stderr = os.Stderr
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

	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	var out json.RawMessage
	if err := ipc.Call(conn, "userbintool.run", json.RawMessage(`{"args":["hello"]}`), &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	var got struct {
		Stdout string `json:"stdout"`
		Exit   int    `json:"exit"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("reply %s: %v", out, err)
	}
	if got.Stdout != "user-bin: hello" || got.Exit != 0 {
		t.Fatalf("got %+v, want the tool in ~/.local/bin to run", got)
	}
}
