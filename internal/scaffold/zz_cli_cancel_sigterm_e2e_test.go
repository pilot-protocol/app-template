//go:build !windows

package scaffold

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// TestCLICancelSendsSIGTERMFirstE2E checks what a cli adapter does to its CLI
// when a call's deadline passes. It used to SIGKILL the CLI outright (exec's
// default), so a CLI never got to stop what it had started itself: with
// io.pilot.aegis, every timed-out `aegis.exec install-models` left its curl
// download running, reparented to init. Now the CLI gets SIGTERM first and
// SIGKILL only killGrace (5 s) later.
//
//   - graceful: a CLI that traps SIGTERM and stops its own child. After the
//     1 s timeout it must have seen SIGTERM, and its child must be gone.
//   - stubborn: a CLI that ignores SIGTERM. It must still be SIGKILLed about
//     killGrace after the timeout, and the call must return.
func TestCLICancelSendsSIGTERMFirstE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	root := t.TempDir()
	tool := filepath.Join(root, "slowtool")
	// $1 = directory for pid/marker files, $2 = mode.
	script := `#!/bin/sh
dir=$1
case "$2" in
graceful)
  sleep 600 >/dev/null 2>&1 &
  kid=$!
  echo "$kid" > "$dir/grandchild.pid"
  trap 'echo term > "$dir/got-term"; kill "$kid"; exit 143' TERM
  wait
  ;;
stubborn)
  trap '' TERM
  echo $$ > "$dir/cli.pid"
  sleep 600 >/dev/null 2>&1 &
  echo $! > "$dir/grandchild.pid"
  while :; do sleep 1; done
  ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := `
id: io.pilot.slowtool
app_version: 0.1.0
description: "Fronts slowtool."
namespace: slowtool
backend:
  type: cli
  command: ["` + tool + `"]
methods:
  - name: slowtool.run
    summary: "Passthrough with a short deadline."
    timeout: 1s
    cli: {passthrough: true}
`
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

	// A short socket path: t.TempDir() can exceed sun_path (104 bytes on macOS).
	sockDir, err := os.MkdirTemp("/tmp", "sigterm-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "app.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adapter never listened")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// call runs one passthrough call and returns how long it took. The reply
	// is only logged: what matters is what happened to the processes.
	call := func(dir, mode string) time.Duration {
		t.Helper()
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		args, _ := json.Marshal(map[string][]string{"args": {dir, mode}})
		start := time.Now()
		var out json.RawMessage
		err = ipc.Call(conn, "slowtool.run", json.RawMessage(args), &out)
		took := time.Since(start)
		t.Logf("%s: reply %s, err %v, after %v", mode, out, err, took)
		return took
	}

	t.Run("graceful", func(t *testing.T) {
		dir := t.TempDir()
		took := call(dir, "graceful")
		kid := sigtermTestPID(t, filepath.Join(dir, "grandchild.pid"))
		t.Cleanup(func() { _ = syscall.Kill(kid, syscall.SIGKILL) })

		if _, err := os.Stat(filepath.Join(dir, "got-term")); err != nil {
			t.Errorf("the CLI never got SIGTERM when its 1s deadline passed (call took %v)", took)
		}
		if !sigtermTestGone(kid, 3*time.Second) {
			t.Errorf("the CLI's own child (pid %d) outlived the timed-out call", kid)
		}
		if took > 4*time.Second {
			t.Errorf("call took %v; a CLI that stops on SIGTERM should end right after the 1s deadline", took)
		}
	})

	t.Run("stubborn", func(t *testing.T) {
		dir := t.TempDir()
		took := call(dir, "stubborn")
		cli := sigtermTestPID(t, filepath.Join(dir, "cli.pid"))
		kid := sigtermTestPID(t, filepath.Join(dir, "grandchild.pid"))
		t.Cleanup(func() {
			_ = syscall.Kill(cli, syscall.SIGKILL)
			_ = syscall.Kill(kid, syscall.SIGKILL)
		})

		if !sigtermTestGone(cli, 3*time.Second) {
			t.Errorf("a CLI that ignores SIGTERM (pid %d) was never SIGKILLed", cli)
		}
		if took < 5*time.Second || took > 10*time.Second {
			t.Errorf("call took %v; want the 1s deadline plus the 5s killGrace", took)
		}
	})
}

func sigtermTestPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sigtermTestGone reports whether pid exits within d. A zombie counts as gone:
// it has exited and only waits to be reaped by its new parent (which, in a
// container without an init, may never happen).
func sigtermTestGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			// Linux: the state letter follows the ")" that closes the command name.
			if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X') {
				return true
			}
		} else if out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); err == nil &&
			strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
