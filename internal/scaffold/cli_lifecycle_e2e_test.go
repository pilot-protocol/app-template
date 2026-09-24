//go:build !windows

package scaffold

import (
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

// TestCLIAdapterChildLifecycleE2E: a CLI child that is still running when its
// call times out, or when the adapter is stopped, must not outlive the call or
// the adapter (io.pilot.miren: `miren login` over miren.exec was left running,
// reparented to init, after the supervisor SIGKILLed the adapter), and a
// timed-out call must fail rather than report a successful {"exit":-1}.
func TestCLIAdapterChildLifecycleE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
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
	spec := `
id: io.pilot.faketool
app_version: 0.1.0
description: "Fronts faketool."
namespace: faketool
backend:
  type: cli
  command: ["` + tool + `"]
methods:
  - name: faketool.hang
    summary: "Blocks until killed."
    timeout: 2s
    cli: {args: ["hang", "${pidfile}"]}
  - name: faketool.run
    summary: "Passthrough."
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

	start := func(t *testing.T) (*exec.Cmd, string) {
		t.Helper()
		// Short path: t.TempDir() for a subtest can exceed sun_path (104 on darwin).
		dir, err := os.MkdirTemp("", "cla")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock := filepath.Join(dir, "app.sock")
		a := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
		a.Stderr = os.Stderr
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // like the supervisor
		if err := a.Start(); err != nil {
			t.Fatalf("start adapter: %v", err)
		}
		t.Cleanup(func() { _ = a.Process.Kill(); _, _ = a.Process.Wait() })
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(sock); err == nil {
				return a, sock
			}
		}
		t.Fatal("adapter socket never appeared")
		return nil, ""
	}
	call := func(sock, method, args string) error {
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		var out json.RawMessage
		return ipc.Call(conn, method, json.RawMessage(args), &out)
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
	gone := func(pid int, within time.Duration) bool {
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

	t.Run("timeout is an error and kills the child", func(t *testing.T) {
		_, sock := start(t)
		pidfile := filepath.Join(t.TempDir(), "pid")
		err := call(sock, "faketool.hang", `{"pidfile":"`+pidfile+`"}`)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("timed-out call: err = %v, want an IPC error mentioning the deadline (not a {\"exit\":-1} result)", err)
		}
		if pid := childPID(t, pidfile); !gone(pid, 3*time.Second) {
			t.Errorf("child pid %d still running after its call timed out", pid)
		}
	})

	stopWith := func(sig syscall.Signal) func(t *testing.T) {
		return func(t *testing.T) {
			a, sock := start(t)
			pidfile := filepath.Join(t.TempDir(), "pid")
			go func() { _ = call(sock, "faketool.run", `{"args":["hang","`+pidfile+`"]}`) }()
			pid := childPID(t, pidfile)
			_ = a.Process.Signal(sig)
			_, _ = a.Process.Wait()
			// SIGTERM: the adapter must not exit before its child is gone.
			// SIGKILL (the supervisor's stop path; also what Pdeathsig does to the
			// adapter when the daemon dies): the kernel's parent-death signal.
			within := 100 * time.Millisecond
			if sig == syscall.SIGKILL {
				within = 2 * time.Second
			}
			if !gone(pid, within) {
				t.Errorf("in-flight child pid %d outlived the adapter (%v)", pid, sig)
			}
		}
	}
	t.Run("SIGTERM stops the in-flight child before exit", stopWith(syscall.SIGTERM))
	t.Run("SIGKILL of the adapter takes the in-flight child down", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("no parent-death signal outside Linux; there the supervisor's process-group stop reaches the child")
		}
		stopWith(syscall.SIGKILL)(t)
	})
}
