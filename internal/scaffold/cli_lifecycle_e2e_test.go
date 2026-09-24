//go:build !windows

package scaffold

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// buildCLIAdapter generates the adapter for spec and builds it, the way
// the other e2e tests do (go.sum seeded from this module, so the build needs no
// network). Returns the binary and the generated project dir.
func buildCLIAdapter(t *testing.T, spec string) (bin, proj string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	cfg := parseSpec(t, spec)
	proj = filepath.Join(root, "proj")
	if _, err := Generate(cfg, proj); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		_ = os.WriteFile(filepath.Join(proj, "go.sum"), sum, 0o644)
	}
	bin = filepath.Join(root, "adapter")
	build := exec.Command("go", "build", "-o", bin, "./cmd/"+cfg.BinaryName)
	build.Dir = proj
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v\n%s", err, out)
	}
	return bin, proj
}

// shortTempDir is a per-test dir with a short path: a t.TempDir() for a
// subtest can exceed sun_path (104 bytes on darwin) once app.sock is appended.
func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// waitForSocket waits until the adapter accepts connections on sock (the file
// appears at bind, a moment before listen).
func waitForSocket(t *testing.T, sock string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
			_ = c.Close()
			return
		}
	}
	t.Fatalf("adapter socket %s did not accept connections within %s", sock, within)
}

// ipcCall dials the adapter once per call, like the supervisor does.
func ipcCall(sock, method, args string, deadline time.Duration) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(deadline))
	var out json.RawMessage
	err = ipc.Call(conn, method, json.RawMessage(args), &out)
	return out, err
}

// processGone reports whether pid has exited within the given time (a zombie
// counts as alive: its parent has not reaped it yet). It SIGKILLs a survivor so
// a failing test leaves nothing behind.
func processGone(pid int, within time.Duration) bool {
	for deadline := time.Now().Add(within); ; time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			return false
		}
	}
}

// TestCLIAdapterChildLifecycleE2E: a CLI child still running when its call
// times out, when the adapter is stopped, or when the process that spawned the
// adapter dies must not outlive the call or the adapter, and a timed-out call
// must fail rather than report a successful {"exit":-1}. io.pilot.miren
// 0.1.0: `miren login` started over miren.exec was left running, reparented to
// init, after the supervisor SIGKILLed the adapter and after the daemon died
// (Linux: the adapter's Pdeathsig is not inherited by its children; macOS: the
// adapter itself kept serving as an orphan), and a call cut off by its 60s
// deadline replied {"exit":-1} as a success.
func TestCLIAdapterChildLifecycleE2E(t *testing.T) {
	root := t.TempDir()
	// `hang <pidfile>` records its pid, then blocks without writing anything
	// (so SIGPIPE cannot end it early once the adapter is gone).
	tool := filepath.Join(root, "faketool")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  hang) echo $$ > \"$2\"; exec sleep 300;;\n" +
		"  *) exit 2;;\n" +
		"esac\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, proj := buildCLIAdapter(t, `
id: io.pilot.faketool
app_version: 0.1.0
description: "Fronts faketool."
namespace: faketool
backend:
  type: cli
  command: ["`+tool+`"]
methods:
  - name: faketool.hang
    summary: "Blocks until killed."
    timeout: 4s # room for the child to start on a loaded host before the deadline
    cli: {args: ["hang", "${pidfile}"]}
  - name: faketool.run
    summary: "Passthrough."
    cli: {passthrough: true}
`)
	manifest := filepath.Join(proj, "manifest.json")

	start := func(t *testing.T) (*exec.Cmd, string) {
		t.Helper()
		sock := filepath.Join(shortTempDir(t, "cla"), "app.sock")
		a := exec.Command(bin, "--socket", sock, "--manifest", manifest)
		a.Stderr = os.Stderr
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // like the supervisor
		if err := a.Start(); err != nil {
			t.Fatalf("start adapter: %v", err)
		}
		t.Cleanup(func() { _ = a.Process.Kill(); _, _ = a.Process.Wait() })
		waitForSocket(t, sock, 10*time.Second)
		return a, sock
	}
	childPID := func(t *testing.T, pidfile string) int {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, err := os.ReadFile(pidfile); err == nil && len(b) > 0 {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					return pid
				}
			}
		}
		t.Fatal("child never started")
		return 0
	}
	hangInFlight := func(t *testing.T, sock string) int {
		t.Helper()
		pidfile := filepath.Join(t.TempDir(), "pid")
		go func() { _, _ = ipcCall(sock, "faketool.run", `{"args":["hang","`+pidfile+`"]}`, 30*time.Second) }()
		return childPID(t, pidfile)
	}

	t.Run("timeout is an error and kills the child", func(t *testing.T) {
		_, sock := start(t)
		pidfile := filepath.Join(t.TempDir(), "pid")
		out, err := ipcCall(sock, "faketool.hang", `{"pidfile":"`+pidfile+`"}`, 30*time.Second)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("timed-out call: out=%s err=%v, want an IPC error naming the deadline (not a {\"exit\":-1} result)", out, err)
		}
		if pid := childPID(t, pidfile); !processGone(pid, 3*time.Second) {
			t.Errorf("child pid %d still running after its call timed out", pid)
		}
	})

	t.Run("SIGTERM stops the in-flight child before the adapter exits", func(t *testing.T) {
		a, sock := start(t)
		pid := hangInFlight(t, sock)
		_ = a.Process.Signal(syscall.SIGTERM)
		if _, err := a.Process.Wait(); err != nil {
			t.Fatal(err)
		}
		if !processGone(pid, 100*time.Millisecond) {
			t.Errorf("in-flight child pid %d outlived the adapter's SIGTERM exit", pid)
		}
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("socket left after a clean exit: %v", err)
		}
	})

	t.Run("SIGKILL of the adapter takes the in-flight child down", func(t *testing.T) {
		// Linux: the child's parent-death signal. Every OS: the child guard
		// (childguard.go), which needs the adapter to lead its process group.
		a, sock := start(t)
		pid := hangInFlight(t, sock)
		_ = a.Process.Kill()
		_, _ = a.Process.Wait()
		if !processGone(pid, 3*time.Second) {
			t.Errorf("in-flight child pid %d outlived the SIGKILLed adapter (%s)", pid, runtime.GOOS)
		}
	})

	t.Run("parent death stops the adapter and its in-flight child", func(t *testing.T) {
		// The adapter's parent is a shell standing in for the supervisor. It is
		// SIGKILLed; the adapter (no Pdeathsig here, as on macOS) must notice,
		// cancel the running call, reap its child and exit.
		sock := filepath.Join(shortTempDir(t, "clp"), "app.sock")
		parent := exec.Command("/bin/sh", "-c", `"$0" "$@" & echo $!; wait`, bin, "--socket", sock, "--manifest", manifest)
		parent.Stderr = os.Stderr
		stdout, err := parent.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := parent.Start(); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			t.Fatalf("read adapter pid: %v", err)
		}
		adapterPID, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("adapter pid %q: %v", line, err)
		}
		t.Cleanup(func() { _ = syscall.Kill(adapterPID, syscall.SIGKILL) })
		waitForSocket(t, sock, 10*time.Second)
		pid := hangInFlight(t, sock)

		_ = parent.Process.Kill()
		_ = parent.Wait()
		if !processGone(adapterPID, 5*time.Second) {
			t.Fatalf("adapter pid %d kept running after its parent died", adapterPID)
		}
		if !processGone(pid, 500*time.Millisecond) {
			t.Errorf("in-flight child pid %d outlived the orphaned adapter", pid)
		}
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("socket left after the adapter shut down: %v", err)
		}
	})
}
