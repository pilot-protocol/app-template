//go:build !windows

package scaffold

import (
	"bytes"
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

// callToolScript is the CLI the test adapter fronts. `spawn <pidfile>` starts a
// long-lived grandchild and waits for it, the way a CLI shell escape (duckdb's
// `.shell sleep 100`) does. `bg <pidfile>` leaves a grandchild running in the
// background and returns at once.
const callToolScript = "#!/bin/sh\n" +
	"case \"$1\" in\n" +
	"  spawn) sleep 300 & echo $! > \"$2\"; wait;;\n" +
	"  bg) sleep 300 >/dev/null 2>&1 & echo $! > \"$2\";;\n" +
	"  *) echo ok;;\n" +
	"esac\n"

type callAdapter struct {
	bin, manifest, appDir, root string
	n                           int
}

func buildCallAdapter(t *testing.T) *callAdapter {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	tool := filepath.Join(root, "calltool")
	if err := os.WriteFile(tool, []byte(callToolScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := parseSpec(t, `
id: io.pilot.calltool
app_version: 0.1.0
description: "Fronts calltool."
namespace: calltool
backend:
  type: cli
  command: ["`+tool+`"]
methods:
  - name: calltool.run
    summary: "Passthrough, default deadline."
    cli: {passthrough: true}
  - name: calltool.quick
    summary: "Passthrough with a 2s deadline."
    timeout: 2s
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
	return &callAdapter{bin: bin, manifest: filepath.Join(proj, "manifest.json"), appDir: proj, root: root}
}

// runningAdapter is one started adapter process.
type runningAdapter struct {
	pid    int
	sock   string
	exited chan struct{} // closed once the adapter has exited and been reaped
}

// start runs the adapter the way the supervisor does: in its own process
// group, with --socket and --manifest.
func (ca *callAdapter) start(t *testing.T) *runningAdapter {
	t.Helper()
	sock := sockPath(t)
	cmd := exec.Command(ca.bin, "--socket", sock, "--manifest", ca.manifest)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	ra := &runningAdapter{pid: cmd.Process.Pid, sock: sock, exited: make(chan struct{})}
	go func() { _, _ = cmd.Process.Wait(); close(ra.exited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-ra.pid, syscall.SIGKILL)
		<-ra.exited
	})
	if !eventually(10*time.Second, func() bool { _, err := os.Stat(sock); return err == nil }) {
		t.Fatal("adapter socket never appeared")
	}
	return ra
}

func (ra *runningAdapter) waitExit(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-ra.exited:
	case <-time.After(d):
		t.Fatalf("adapter pid %d still running %s later", ra.pid, d)
	}
}

func callTool(sock, method, args string) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
	var out json.RawMessage
	err = ipc.Call(conn, method, json.RawMessage(args), &out)
	return out, err
}

// startCall runs `calltool <sub> <pidfile>` through method in the background
// and returns the grandchild's pid once it is running, and the call's result.
func (ca *callAdapter) startCall(t *testing.T, sock, method, sub string) (int, chan error) {
	t.Helper()
	ca.n++
	pidfile := filepath.Join(ca.root, "pid-"+strconv.Itoa(ca.n))
	done := make(chan error, 1)
	go func() {
		_, err := callTool(sock, method, `{"args":["`+sub+`","`+pidfile+`"]}`)
		done <- err
	}()
	var pid int
	if !eventually(10*time.Second, func() bool {
		b, err := os.ReadFile(pidfile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	}) {
		t.Fatal("the call's grandchild never started")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid, done
}

// gone reports whether pid has exited. A zombie awaiting its reaper counts: a
// reparented process in a container without an init may never be reaped.
func gone(pid int) bool {
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return true
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := bytes.LastIndexByte(b, ')'); i > 0 && i+2 < len(b) {
			return b[i+2] == 'Z'
		}
	}
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func mustGo(t *testing.T, pid int, d time.Duration, what string) {
	t.Helper()
	if !eventually(d, func() bool { return gone(pid) }) {
		t.Fatalf("%s: pid %d still running %s later", what, pid, d)
	}
}

// guardianOf returns the pid of the call guardian the adapter started, or 0.
func guardianOf(adapterPID int) int {
	if ents, err := os.ReadDir("/proc"); err == nil {
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			stat, _ := os.ReadFile("/proc/" + e.Name() + "/stat")
			cmdline, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			i := bytes.LastIndexByte(stat, ')')
			if i < 0 {
				continue
			}
			f := bytes.Fields(stat[i+1:])
			if len(f) > 1 && string(f[1]) == strconv.Itoa(adapterPID) && bytes.Contains(cmdline, []byte("--pilot-call-guardian")) {
				return pid
			}
		}
		return 0
	}
	out, _ := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "command=").Output()
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == strconv.Itoa(adapterPID) && strings.Contains(line, "--pilot-call-guardian") {
			pid, _ := strconv.Atoi(f[0])
			return pid
		}
	}
	return 0
}

// TestCLICallProcessesNeverOutliveTheCall runs a real generated cli adapter and
// checks that nothing a call starts is left running: not when the call hits its
// deadline, not when the adapter is stopped or its parent dies, not when the
// adapter is SIGKILLed (pid or group, as the supervisor stops apps), and, if
// the guardian was killed too, not past the adapter's next start. It also
// checks that a stale record never kills a process group that is not the call.
func TestCLICallProcessesNeverOutliveTheCall(t *testing.T) {
	ca := buildCallAdapter(t)

	t.Run("deadline kills the call's process tree and is an IPC error", func(t *testing.T) {
		sock := ca.start(t).sock
		pid, done := ca.startCall(t, sock, "calltool.quick", "spawn")
		var err error
		select {
		case err = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("call did not return after its 2s deadline")
		}
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Errorf("want an IPC error naming the deadline, got %v", err)
		}
		mustGo(t, pid, 3*time.Second, "grandchild after the call's deadline")
	})

	t.Run("caller hanging up kills the call's process tree", func(t *testing.T) {
		sock := ca.start(t).sock
		ca.n++
		pidfile := filepath.Join(ca.root, "pid-"+strconv.Itoa(ca.n))
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		req := &ipc.Envelope{Type: ipc.EnvReq, ReqID: "r1", Method: "calltool.run", Payload: json.RawMessage(`{"args":["spawn","` + pidfile + `"]}`)}
		if err := ipc.WriteFrame(conn, req); err != nil {
			t.Fatal(err)
		}
		var pid int
		if !eventually(10*time.Second, func() bool {
			b, err := os.ReadFile(pidfile)
			if err == nil {
				pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			}
			return err == nil && pid > 0
		}) {
			t.Fatal("the call's grandchild never started")
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		_ = conn.Close()
		mustGo(t, pid, 3*time.Second, "grandchild after its caller hung up")
	})

	t.Run("SIGTERM kills running calls and background leftovers before the adapter exits", func(t *testing.T) {
		ra := ca.start(t)
		sock := ra.sock
		running, _ := ca.startCall(t, sock, "calltool.run", "spawn")
		left, done := ca.startCall(t, sock, "calltool.run", "bg")
		if err := <-done; err != nil {
			t.Fatalf("bg call: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if gone(left) {
			t.Fatal("a returned call's background process was killed while the adapter runs")
		}
		guard := guardianOf(ra.pid)
		if guard == 0 {
			t.Fatal("no call guardian is running beside the adapter")
		}
		_ = syscall.Kill(ra.pid, syscall.SIGTERM)
		ra.waitExit(t, 10*time.Second)
		mustGo(t, running, time.Second, "running call's grandchild after SIGTERM")
		mustGo(t, left, time.Second, "returned call's background process after SIGTERM")
		mustGo(t, guard, 3*time.Second, "call guardian after the adapter exited")
		if ents, _ := os.ReadDir(filepath.Join(ca.appDir, ".calls")); len(ents) != 0 {
			t.Errorf("call records left after a clean stop: %d", len(ents))
		}
	})

	t.Run("parent death kills running calls", func(t *testing.T) {
		sock := sockPath(t)
		pidfile := filepath.Join(ca.root, "adapter.pid")
		// sh plays the supervisor: it starts the adapter and is then SIGKILLed.
		parent := exec.Command("/bin/sh", "-c", `"$0" --socket "$1" --manifest "$2" & echo $! > "$3"; wait`, ca.bin, sock, ca.manifest, pidfile)
		parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := parent.Start(); err != nil {
			t.Fatal(err)
		}
		var apid int
		if !eventually(10*time.Second, func() bool {
			b, err := os.ReadFile(pidfile)
			if err == nil {
				apid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			}
			return err == nil && apid > 0
		}) {
			t.Fatal("adapter never started")
		}
		t.Cleanup(func() { _ = syscall.Kill(apid, syscall.SIGKILL) })
		if !eventually(10*time.Second, func() bool { _, err := os.Stat(sock); return err == nil }) {
			t.Fatal("adapter socket never appeared")
		}
		pid, _ := ca.startCall(t, sock, "calltool.run", "spawn")
		_ = parent.Process.Kill()
		_, _ = parent.Process.Wait()
		mustGo(t, apid, 5*time.Second, "adapter after its parent died")
		mustGo(t, pid, 3*time.Second, "grandchild after the adapter's parent died")
	})

	for _, target := range []string{"pid", "group"} {
		t.Run("SIGKILL to the adapter "+target+": the guardian kills running calls and leftovers", func(t *testing.T) {
			ra := ca.start(t)
			sock := ra.sock
			running, _ := ca.startCall(t, sock, "calltool.run", "spawn")
			left, done := ca.startCall(t, sock, "calltool.run", "bg")
			if err := <-done; err != nil {
				t.Fatalf("bg call: %v", err)
			}
			guard := guardianOf(ra.pid)
			kill := ra.pid
			if target == "group" {
				kill = -kill
			}
			_ = syscall.Kill(kill, syscall.SIGKILL)
			ra.waitExit(t, 5*time.Second)
			mustGo(t, running, 2*time.Second, "running call's grandchild after the adapter was SIGKILLed")
			mustGo(t, left, 2*time.Second, "returned call's background process after the adapter was SIGKILLed")
			if guard != 0 {
				mustGo(t, guard, 2*time.Second, "call guardian after it cleaned up")
			}
		})
	}

	t.Run("adapter and guardian both SIGKILLed: the next start reaps the calls", func(t *testing.T) {
		ra := ca.start(t)
		sock := ra.sock
		pid, _ := ca.startCall(t, sock, "calltool.run", "spawn")
		guard := guardianOf(ra.pid)
		if guard == 0 {
			t.Fatal("no call guardian is running beside the adapter")
		}
		_ = syscall.Kill(guard, syscall.SIGKILL)
		mustGo(t, guard, 2*time.Second, "guardian")
		_ = syscall.Kill(ra.pid, syscall.SIGKILL)
		ra.waitExit(t, 5*time.Second)
		time.Sleep(500 * time.Millisecond)
		if gone(pid) {
			t.Fatal("grandchild died with nothing left to kill it; the scenario did not happen")
		}
		ca.start(t)
		mustGo(t, pid, 3*time.Second, "grandchild after the next start")
	})

	t.Run("an idle adapter runs no guardian", func(t *testing.T) {
		ra := ca.start(t)
		for i := 0; i < 3; i++ {
			if out, err := callTool(ra.sock, "calltool.run", `{"args":["x"]}`); err != nil || !strings.Contains(string(out), "ok") {
				t.Fatalf("call: %s %v", out, err)
			}
		}
		if !eventually(3*time.Second, func() bool { return guardianOf(ra.pid) == 0 }) {
			t.Fatalf("call guardian pid %d still running 3s after the last call returned", guardianOf(ra.pid))
		}
		if ents, _ := os.ReadDir(filepath.Join(ca.appDir, ".calls")); len(ents) != 0 {
			t.Errorf("call records left after the calls returned: %d", len(ents))
		}
	})

	t.Run("a stale record never kills a group that is not the call", func(t *testing.T) {
		// An unrelated process group whose id sits in the call records, as if
		// the id had been reused after the recorded call ended.
		other := exec.Command("sleep", "300")
		other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Process.Kill(); _, _ = other.Process.Wait() })
		calls := filepath.Join(ca.appDir, ".calls")
		_ = os.MkdirAll(calls, 0o700)
		rec := filepath.Join(calls, strconv.Itoa(other.Process.Pid))
		if err := os.WriteFile(rec, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ra := ca.start(t)
		sock := ra.sock
		if _, err := os.Stat(rec); err == nil {
			t.Error("the stale record was not cleared on start")
		}
		if out, err := callTool(sock, "calltool.run", `{"args":["x"]}`); err != nil || !strings.Contains(string(out), "ok") {
			t.Fatalf("call: %s %v", out, err)
		}
		_ = syscall.Kill(ra.pid, syscall.SIGKILL)
		ra.waitExit(t, 5*time.Second)
		time.Sleep(300 * time.Millisecond)
		if syscall.Kill(other.Process.Pid, 0) != nil {
			t.Fatal("an unrelated process group named by a stale record was killed")
		}
	})
}
