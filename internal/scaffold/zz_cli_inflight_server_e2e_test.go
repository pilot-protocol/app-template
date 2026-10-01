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

// miniSupEnv switches the test binary into a stand-in for the pilot daemon's
// supervisor (TestHelperMiniSupervisor): it spawns the adapter named in the
// variable with the supervisor's process attributes, prints the adapter's pid
// and then waits to be killed.
const miniSupEnv = "SCAFFOLD_TEST_MINI_SUPERVISOR"

// TestHelperMiniSupervisor is not a test: it is the helper process for
// TestCLIInflightServerE2E and skips unless miniSupEnv is set.
func TestHelperMiniSupervisor(t *testing.T) {
	spec := os.Getenv(miniSupEnv)
	if spec == "" {
		t.Skip("helper process for TestCLIInflightServerE2E")
	}
	var argv []string
	if err := json.Unmarshal([]byte(spec), &argv); err != nil || len(argv) == 0 {
		t.Fatalf("bad %s: %q", miniSupEnv, spec)
	}
	// app-store spawn(): own process group everywhere, plus Pdeathsig=SIGKILL
	// on Linux from a thread that stays alive as long as this process does.
	runtime.LockOSThread()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = superviseAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn adapter: %v", err)
	}
	os.Stdout.WriteString(strconv.Itoa(cmd.Process.Pid) + "\n")
	time.Sleep(time.Hour) // until the test SIGKILLs us, as a crashed daemon disappears
}

// TestCLIInflightServerE2E: the io.pilot.otto shape. A passthrough call starts
// a long-running server (`otto.exec ["mcp","serve-http","--port",P]`) that is
// still running when the app goes away. In otto 0.20.0 the child kept its port
// open, reparented to init/launchd, after the daemon died (Linux: the
// adapter's Pdeathsig killed the adapter, not the child; macOS: the adapter
// itself was orphaned and kept serving) and, on macOS, after the adapter was
// SIGKILLed (the supervisor's stop path). The child guard (childguard.go)
// covers the SIGKILL case on every OS; these subtests pin its lifecycle too.
func TestCLIInflightServerE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	tool := filepath.Join(root, "srvtool")
	// `serve <pidfile>` records its pid and then runs until killed, without
	// writing anything (so a closed pipe cannot end it early).
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  serve) echo $$ > \"$2\"; exec sleep 300;;\n" +
		"  quick) echo '{\"ok\":true}';;\n" +
		"  *) echo '{}';;\n" +
		"esac\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := parseSpec(t, `
id: io.pilot.srvtool
app_version: 0.1.0
description: "Fronts srvtool."
namespace: srvtool
backend:
  type: cli
  command: ["`+tool+`"]
methods:
  - name: srvtool.exec
    summary: "Passthrough."
    timeout: 120s
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

	shortDir := func(t *testing.T) string {
		t.Helper()
		dir, err := os.MkdirTemp("", "cis") // short: sun_path is ~104 bytes on darwin
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	waitSock := func(t *testing.T, sock string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(sock); err == nil {
				return
			}
		}
		t.Fatal("adapter socket never appeared")
	}
	// startServe issues the long-running passthrough call without waiting for
	// it and returns the server child's pid.
	startServe := func(t *testing.T, sock, dir string) int {
		t.Helper()
		pidfile := filepath.Join(dir, "child.pid")
		go func() {
			conn, err := net.DialTimeout("unix", sock, 3*time.Second)
			if err != nil {
				return
			}
			defer conn.Close()
			var out json.RawMessage
			_ = ipc.Call(conn, "srvtool.exec", map[string]any{"args": []string{"serve", pidfile}}, &out)
		}()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, err := os.ReadFile(pidfile); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
					t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
					return pid
				}
			}
		}
		t.Fatal("server child never started")
		return 0
	}
	gone := func(pid int, within time.Duration) bool {
		for deadline := time.Now().Add(within); ; time.Sleep(20 * time.Millisecond) {
			if !procAlive(pid) {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
		}
	}

	t.Run("daemon dies with a server call in flight", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		argv, _ := json.Marshal([]string{bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json")})
		sup := exec.Command(os.Args[0], "-test.run=^TestHelperMiniSupervisor$", "-test.v=false")
		sup.Env = append(os.Environ(), miniSupEnv+"="+string(argv))
		sup.Stderr = os.Stderr
		out, err := sup.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := sup.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sup.Process.Kill(); _, _ = sup.Process.Wait() })
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil {
			t.Fatalf("read adapter pid from mini supervisor: %v", err)
		}
		apid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("adapter pid %q: %v", line, err)
		}
		t.Cleanup(func() { _ = syscall.Kill(-apid, syscall.SIGKILL) })
		waitSock(t, sock)
		child := startServe(t, sock, dir)

		_ = sup.Process.Kill() // the daemon crashes
		_, _ = sup.Process.Wait()
		// Linux: the adapter's Pdeathsig kills it and the child's own
		// Pdeathsig kills the child. macOS: the adapter sees it was reparented,
		// shuts down and reaps the child before exiting.
		if !gone(apid, 5*time.Second) {
			t.Errorf("adapter pid %d outlived its supervisor", apid)
		}
		if !gone(child, 5*time.Second) {
			t.Errorf("server child pid %d outlived the dead supervisor and adapter (reparented to init/launchd)", child)
		}
	})

	// startAdapter runs the adapter the way the supervisor does: leader of its
	// own process group.
	startAdapter := func(t *testing.T, dir string) (*exec.Cmd, string) {
		t.Helper()
		sock := filepath.Join(dir, "app.sock")
		a := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
		a.Stderr = os.Stderr
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		apid := a.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-apid, syscall.SIGKILL); _, _ = a.Process.Wait() })
		waitSock(t, sock)
		return a, sock
	}

	t.Run("adapter SIGKILLed with a server call in flight", func(t *testing.T) {
		// The app-store supervisor's stop path through v1.0.3: SIGKILL to the
		// adapter's pid only. Linux: the child's Pdeathsig. Everywhere
		// (macOS has no Pdeathsig): the child guard takes the group down.
		dir := shortDir(t)
		a, sock := startAdapter(t, dir)
		apid := a.Process.Pid
		child := startServe(t, sock, dir)
		_ = syscall.Kill(apid, syscall.SIGKILL)
		_, _ = a.Process.Wait()
		if !gone(child, 3*time.Second) {
			t.Errorf("server child pid %d outlived the SIGKILLed adapter", child)
		}
		if left := groupLeft(apid, 3*time.Second); len(left) > 0 {
			t.Errorf("adapter's process group %d not empty after it was SIGKILLed: %v", apid, left)
		}
	})

	t.Run("clean stop leaves an empty process group", func(t *testing.T) {
		dir := shortDir(t)
		a, sock := startAdapter(t, dir)
		apid := a.Process.Pid
		pidfile := filepath.Join(dir, "quick.pid")
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var out json.RawMessage
		if err := ipc.Call(conn, "srvtool.exec", map[string]any{"args": []string{"quick", pidfile}}, &out); err != nil {
			t.Fatalf("quick call: %v", err)
		}
		conn.Close()
		// The guard outlives the call briefly (guardIdle), so the group is not
		// empty yet; a SIGTERM now must still leave nothing behind.
		if left := groupMembers(apid); len(left) < 2 {
			t.Fatalf("expected the adapter and its idle child guard in group %d, got %v", apid, left)
		}
		_ = a.Process.Signal(syscall.SIGTERM)
		_, _ = a.Process.Wait()
		// CloseChildGuard released and reaped the guard before the adapter exited.
		if left := groupMembers(apid); len(left) > 0 {
			t.Errorf("process group %d not empty after a clean SIGTERM: %v", apid, left)
		}
	})

	t.Run("idle adapter retires its guard", func(t *testing.T) {
		dir := shortDir(t)
		a, sock := startAdapter(t, dir)
		apid := a.Process.Pid
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var out json.RawMessage
		if err := ipc.Call(conn, "srvtool.exec", map[string]any{"args": []string{"quick"}}, &out); err != nil {
			t.Fatalf("quick call: %v", err)
		}
		conn.Close()
		deadline := time.Now().Add(6 * time.Second)
		for len(groupMembers(apid)) > 1 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if left := groupMembers(apid); len(left) != 1 || left[0] != apid {
			t.Fatalf("idle adapter group %d = %v, want only the adapter (guard retired)", apid, left)
		}
		if !procAlive(apid) {
			t.Fatal("adapter died when its guard retired")
		}
	})

	t.Run("adapter keeps serving when its guard dies", func(t *testing.T) {
		dir := shortDir(t)
		a, sock := startAdapter(t, dir)
		apid := a.Process.Pid
		child := startServe(t, sock, dir)
		var guardPID int
		for _, pid := range groupMembers(apid) {
			if pid != apid && pid != child {
				guardPID = pid
			}
		}
		if guardPID == 0 {
			t.Fatalf("no child guard in group %d: %v", apid, groupMembers(apid))
		}
		_ = syscall.Kill(guardPID, syscall.SIGKILL)
		if !gone(guardPID, 3*time.Second) {
			t.Fatalf("guard %d did not die", guardPID)
		}
		if !procAlive(apid) || !procAlive(child) {
			t.Fatalf("killing the guard took down adapter (alive=%v) or child (alive=%v)", procAlive(apid), procAlive(child))
		}
		// The next call starts a new guard, which still covers the old child.
		second := startServe(t, sock, shortDir(t))
		_ = syscall.Kill(apid, syscall.SIGKILL)
		_, _ = a.Process.Wait()
		for _, pid := range []int{child, second} {
			if !gone(pid, 3*time.Second) {
				t.Errorf("child %d outlived the SIGKILLed adapter after its first guard died", pid)
			}
		}
	})

	t.Run("supervisor group stop reaches the server child", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		a := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
		a.Stderr = os.Stderr
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as the supervisor spawns it
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		apid := a.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-apid, syscall.SIGKILL); _, _ = a.Process.Wait() })
		waitSock(t, sock)
		child := startServe(t, sock, dir)

		// The child must stay in the adapter's process group: on macOS (no
		// Pdeathsig) a group-wide signal from the supervisor is what reaches it
		// when the adapter is SIGKILLed.
		if pg, err := syscall.Getpgid(child); err != nil || pg != apid {
			t.Fatalf("server child pgid = %d (err %v), want the adapter's group %d", pg, err, apid)
		}
		if err := syscall.Kill(-apid, syscall.SIGKILL); err != nil {
			t.Fatalf("kill group: %v", err)
		}
		_, _ = a.Process.Wait()
		if !gone(child, 3*time.Second) {
			t.Errorf("server child pid %d survived a SIGKILL of the adapter's process group", child)
		}
	})
}

// procAlive reports whether pid is a live (non-zombie) process. A zombie is
// dead for this purpose: in a container nothing may ever reap it.
func procAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) && (s[i+2] == 'Z' || s[i+2] == 'X') {
			return false
		}
		return true
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
	}
	return true
}

// groupLeft waits up to d for process group pgid to empty and returns what is
// still in it.
func groupLeft(pgid int, d time.Duration) []int {
	deadline := time.Now().Add(d)
	for {
		left := groupMembers(pgid)
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// groupMembers lists the live (non-zombie) members of process group pgid.
func groupMembers(pgid int) []int {
	// ps is on every darwin host and in the Linux test images (procps).
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,stat=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		pg, _ := strconv.Atoi(f[1])
		if pg == pgid && !strings.HasPrefix(f[2], "Z") {
			pids = append(pids, pid)
		}
	}
	return pids
}
