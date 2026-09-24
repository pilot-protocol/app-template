//go:build !windows

package scaffold

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// slowListenerEnv switches the test binary into a server that starts
// listening only after a delay, as a freshly staged binary does on its first
// launch (macOS assesses it, Rosetta translates it): TestHelperSlowListener.
const slowListenerEnv = "SCAFFOLD_TEST_SLOW_LISTENER"

// TestHelperSlowListener is not a test: it is the `listen` tool of
// TestCLIServiceLifetime and skips unless slowListenerEnv is set. Args (after
// --): --port P (or --port=P) --delay D --pidfile F; it listens on
// 127.0.0.1:P after D and runs until SIGTERM.
func TestHelperSlowListener(t *testing.T) {
	if os.Getenv(slowListenerEnv) == "" {
		t.Skip("helper process for TestCLIServiceLifetime")
	}
	args := flag.Args()
	val := func(name string) string {
		v := ""
		for i, a := range args {
			if a == name && i+1 < len(args) {
				v = args[i+1]
			} else if strings.HasPrefix(a, name+"=") {
				v = strings.TrimPrefix(a, name+"=")
			}
		}
		return v
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	_ = os.WriteFile(val("--pidfile"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	delay, _ := time.ParseDuration(val("--delay"))
	select {
	case <-sig:
		os.Exit(0)
	case <-time.After(delay):
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+val("--port"))
	if err != nil {
		os.Exit(3)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	<-sig
	os.Exit(0)
}

// TestCLIServiceLifetime: a server started by a cli.service method must not
// outlive the adapter, however the adapter goes away. Found with io.pilot.redis
// 8.6.2: redis.start ran `redis-server --daemonize yes`, whose server setsid()s
// out of the adapter's process group and survived the adapter's SIGTERM, the
// supervisor's SIGKILL of the adapter's pid, and the supervisor's own death, on
// Linux and macOS. A passthrough redis.exec could do the same, or run any host
// binary through a relative args[0] ("../../../../bin/sh").
//
//   - the server stays a foreground child in the adapter's group, with
//     force_args appended last (so --daemonize no always wins);
//   - SIGTERM: StopServices stops it, and the group is left empty;
//   - SIGKILL of the adapter: the child guard, held while the server runs,
//     takes the group down (the only mechanism on macOS; Linux adds Pdeathsig);
//   - supervisor death: the adapter shuts down (macOS: parent watch; Linux: the
//     supervisor's Pdeathsig kills the adapter, the server's Pdeathsig and the
//     guard do the rest);
//   - passthrough: args[0] must be in cli.tools; a one-shot service tool
//     (--version) replies like a command; a busy ready_tcp fails fast; a server
//     that never gets ready is stopped.
func TestCLIServiceLifetime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	tool := filepath.Join(root, "svctool")
	// `serve <pidfile> ...` records "pid argv" and runs until signalled; a
	// SIGTERM (a clean stop, which lets a real server save its data) leaves
	// <pidfile>.term behind, a SIGKILL leaves nothing. `quick` is a one-shot
	// that exits before it could count as ready.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  serve) echo \"$$ $*\" > \"$2\"; trap 'echo term > \"$2.term\"; kill $! 2>/dev/null; exit 0' TERM; sleep 300 & wait;;\n" +
		"  servei) echo \"$$ $*\" > \"$2\"; trap 'echo int > \"$2.int\"; kill $! 2>/dev/null; exit 0' INT; trap 'echo term > \"$2.term\"; kill $! 2>/dev/null; exit 0' TERM; sleep 300 & wait;;\n" +
		"  quick) echo quick-out; exit 3;;\n" +
		"  listen) shift; exec env " + slowListenerEnv + "=1 '" + os.Args[0] + "' -test.run='^TestHelperSlowListener$' -- \"$@\";;\n" +
		"  *) echo '{}';;\n" +
		"esac\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := parseSpec(t, `
id: io.pilot.svctool
app_version: 0.1.0
description: "Fronts svctool."
namespace: svctool
backend:
  type: cli
  command: ["`+tool+`"]
methods:
  - name: svctool.start
    summary: "Start a server."
    cli:
      args: ["serve", "${pidfile}"]
      service: {ready_after: "300ms", ready_timeout: "10s", force_args: ["--daemonize", "no"]}
  - name: svctool.startint
    summary: "Start a server whose fast clean stop is SIGINT."
    cli:
      args: ["servei", "${pidfile}"]
      service: {ready_after: "300ms", ready_timeout: "10s", stop_signal: "SIGINT"}
  - name: svctool.listen
    summary: "Start a server that must answer on a port."
    cli:
      args: ["serve", "${pidfile}"]
      service: {ready_tcp: "127.0.0.1:${port}", ready_timeout: "2s"}
  - name: svctool.exec
    summary: "Passthrough."
    cli:
      passthrough: true
      tools: ["serve", "quick", "listen"]
      service: {tools: ["serve", "listen"], ready_after: "300ms", ready_port_flag: "--port", force_args: ["--daemonize", "no"]}
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
	manifest := filepath.Join(proj, "manifest.json")

	shortDir := func(t *testing.T) string {
		t.Helper()
		dir, err := os.MkdirTemp("", "svc") // short: sun_path is ~104 bytes on darwin
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	waitFor := func(t *testing.T, cond func() bool, d time.Duration, what string) {
		t.Helper()
		for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
			if cond() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out after %s waiting for %s", d, what)
			}
		}
	}
	waitSock := func(t *testing.T, sock string) {
		t.Helper()
		waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil }, 10*time.Second, "adapter socket")
	}
	// spawn starts the adapter as the supervisor does: leader of its own group.
	spawn := func(t *testing.T, sock string) *exec.Cmd {
		t.Helper()
		a := exec.Command(bin, "--socket", sock, "--manifest", manifest)
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		a.Stderr = os.Stderr
		if err := a.Start(); err != nil {
			t.Fatalf("start adapter: %v", err)
		}
		pid := a.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = a.Process.Wait() })
		waitSock(t, sock)
		return a
	}
	call := func(sock, method string, args any) (map[string]any, error) {
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		var out json.RawMessage
		if err := ipc.Call(conn, method, args, &out); err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return m, nil
	}
	readPid := func(t *testing.T, pidFile string) (int, string) {
		t.Helper()
		var pid int
		var line string
		waitFor(t, func() bool {
			b, err := os.ReadFile(pidFile)
			if err != nil {
				return false
			}
			line = strings.TrimSpace(string(b))
			f := strings.Fields(line)
			if len(f) == 0 {
				return false
			}
			pid, err = strconv.Atoi(f[0])
			return err == nil && pid > 0
		}, 10*time.Second, "server pid file")
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		return pid, line
	}
	startArgs := func(method, pidFile string) any {
		if method == "svctool.exec" {
			return map[string]any{"args": []string{"serve", pidFile}}
		}
		return map[string]string{"pidfile": pidFile}
	}

	stops := []struct {
		name string
		stop func(t *testing.T, a *exec.Cmd)
	}{
		{"sigterm", func(t *testing.T, a *exec.Cmd) {
			_ = a.Process.Signal(syscall.SIGTERM)
			st, _ := a.Process.Wait()
			if st == nil || !st.Success() {
				t.Errorf("adapter exit after SIGTERM = %v, want 0", st)
			}
		}},
		// The app-store supervisor's stop path through v1.0.3: SIGKILL to the
		// adapter's pid only.
		{"sigkill", func(t *testing.T, a *exec.Cmd) {
			_ = a.Process.Kill()
			_, _ = a.Process.Wait()
		}},
	}
	for _, tc := range stops {
		for _, method := range []string{"svctool.start", "svctool.exec"} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				dir := shortDir(t)
				sock := filepath.Join(dir, "app.sock")
				pidFile := filepath.Join(dir, "srv.pid")
				a := spawn(t, sock)
				apid := a.Process.Pid
				res, err := call(sock, method, startArgs(method, pidFile))
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				if res["exit"] != float64(0) || res["pid"] == nil {
					t.Fatalf("%s reply = %v, want exit 0 with a pid", method, res)
				}
				srv, argv := readPid(t, pidFile)
				if !strings.HasSuffix(argv, "--daemonize no") {
					t.Errorf("server argv %q: force_args must come last", argv)
				}
				if pg, err := syscall.Getpgid(srv); err != nil || pg != apid {
					t.Errorf("server pgid = %d (err %v), want the adapter's group %d", pg, err, apid)
				}
				// A second call while the first server runs: the adapter keeps
				// serving, and the guard it holds does not end the call.
				if res, err := call(sock, "svctool.exec", map[string]any{"args": []string{"quick"}}); err != nil || res["exit"] != float64(3) {
					t.Errorf("quick while a server runs = %v, %v", res, err)
				}
				// Past the guard's idle retirement (guardIdle, 1s) after that
				// call: from here on only the running server keeps a guard.
				time.Sleep(1500 * time.Millisecond)
				if n := len(groupMembers(apid)); n < 3 {
					t.Errorf("group %d has %d live members, want adapter + server + child guard", apid, n)
				}
				tc.stop(t, a)
				waitFor(t, func() bool { return !procAlive(srv) }, 10*time.Second, "server to die after adapter "+tc.name)
				if left := groupLeft(apid, 3*time.Second); len(left) > 0 {
					t.Errorf("adapter's process group %d not empty after %s: %v", apid, tc.name, left)
				}
				// A clean stop of the adapter stops its server cleanly too
				// (SIGTERM from StopServices), not by the guard's SIGKILL.
				if _, err := os.Stat(pidFile + ".term"); tc.name == "sigterm" && err != nil {
					t.Errorf("server was not stopped with SIGTERM on the adapter's clean exit (no %s)", filepath.Base(pidFile)+".term")
				}
			})
		}
	}

	t.Run("supervisor-death", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		pidFile := filepath.Join(dir, "srv.pid")
		argv, _ := json.Marshal([]string{bin, "--socket", sock, "--manifest", manifest})
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
		if res, err := call(sock, "svctool.start", map[string]string{"pidfile": pidFile}); err != nil || res["exit"] != float64(0) {
			t.Fatalf("svctool.start = %v, %v", res, err)
		}
		srv, _ := readPid(t, pidFile)
		_ = sup.Process.Kill() // the daemon crashes
		_, _ = sup.Process.Wait()
		waitFor(t, func() bool { return !procAlive(apid) }, 5*time.Second, "adapter to exit after its supervisor died")
		waitFor(t, func() bool { return !procAlive(srv) }, 10*time.Second, "server to stop with the adapter")
		if left := groupLeft(apid, 3*time.Second); len(left) > 0 {
			t.Errorf("adapter's process group %d not empty after supervisor death: %v", apid, left)
		}
	})

	t.Run("replies", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		spawn(t, sock)
		// A one-shot passthrough service tool returns its output like a command.
		res, err := call(sock, "svctool.exec", map[string]any{"args": []string{"quick"}})
		if err != nil || res["exit"] != float64(3) || res["stdout"] != "quick-out" {
			t.Errorf("quick = %v, %v; want exit 3 stdout quick-out", res, err)
		}
		// args[0] outside cli.tools (a path, a shell, nothing) is refused.
		for _, bad := range [][]string{{"../../../../bin/sh", "-c", "true"}, {"sh"}, {"./serve"}, {}} {
			if _, err := call(sock, "svctool.exec", map[string]any{"args": bad}); err == nil || !strings.Contains(err.Error(), "args[0] must be one of") {
				t.Errorf("args %q: err = %v, want the tools allowlist refusal", bad, err)
			}
		}
		// ready_tcp: something already listening on the port fails fast and
		// starts nothing.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		pf := filepath.Join(dir, "busy.pid")
		if _, err := call(sock, "svctool.listen", map[string]string{"pidfile": pf, "port": port}); err == nil || !strings.Contains(err.Error(), "already accepting connections") {
			t.Errorf("listen on a busy port: err = %v, want already-accepting refusal", err)
		}
		if _, err := os.Stat(pf); err == nil {
			t.Errorf("server was started although the port was busy")
		}
		// ready_tcp never satisfied: the server is stopped and the call fails.
		ln2, _ := net.Listen("tcp", "127.0.0.1:0")
		free := strconv.Itoa(ln2.Addr().(*net.TCPAddr).Port)
		ln2.Close()
		pf2 := filepath.Join(dir, "never.pid")
		if _, err := call(sock, "svctool.listen", map[string]string{"pidfile": pf2, "port": free}); err == nil || !strings.Contains(err.Error(), "not ready within") {
			t.Errorf("never-ready server: err = %v, want not-ready failure", err)
		}
		srv, _ := readPid(t, pf2)
		waitFor(t, func() bool { return !procAlive(srv) }, 12*time.Second, "never-ready server to be stopped")
	})

	// Passthrough readiness follows the server's --port: a server that takes
	// longer than ready_after to listen (a first launch) is only reported
	// started once 127.0.0.1:<port> accepts, and a busy port fails fast.
	t.Run("passthrough-ready-port", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		spawn(t, sock)
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		ln.Close()
		for i, portArgs := range [][]string{{"--port", port}, {"--port=" + port}} {
			pf := filepath.Join(dir, fmt.Sprintf("l%d.pid", i))
			args := append([]string{"listen", "--delay", "1500ms", "--pidfile", pf}, portArgs...)
			t0 := time.Now()
			res, err := call(sock, "svctool.exec", map[string]any{"args": args})
			if err != nil || res["exit"] != float64(0) {
				t.Fatalf("listen %v = %v, %v", portArgs, res, err)
			}
			if took := time.Since(t0); took < 1400*time.Millisecond {
				t.Errorf("listen %v returned after %s, before the server listened (1.5s)", portArgs, took)
			}
			if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err != nil {
				t.Errorf("listen %v reported started but 127.0.0.1:%s refuses: %v", portArgs, port, err)
			} else {
				c.Close()
			}
			if !strings.Contains(fmt.Sprint(res["stdout"]), "accepting connections on 127.0.0.1:"+port) {
				t.Errorf("reply %v does not name the address it waited for", res)
			}
			// The same port again: already accepting, so nothing starts.
			pf2 := filepath.Join(dir, fmt.Sprintf("l%d-busy.pid", i))
			if _, err := call(sock, "svctool.exec", map[string]any{"args": []string{"listen", "--port", port, "--delay", "0s", "--pidfile", pf2}}); err == nil || !strings.Contains(err.Error(), "already accepting connections") {
				t.Errorf("second listen on %s: err = %v, want already-accepting refusal", port, err)
			}
			if _, err := os.Stat(pf2); err == nil {
				t.Errorf("a server was started on the busy port %s", port)
			}
			// Stop this one before the next form reuses the port.
			pid, _ := readPid(t, pf)
			_ = syscall.Kill(pid, syscall.SIGTERM)
			waitFor(t, func() bool { return !procAlive(pid) }, 5*time.Second, "listener to stop")
		}
	})

	// Several services at once all stop on SIGTERM, within the grace.
	t.Run("sigterm/many", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		a := spawn(t, sock)
		apid := a.Process.Pid
		var srvs []int
		for i := 0; i < 3; i++ {
			pf := filepath.Join(dir, fmt.Sprintf("m%d.pid", i))
			if _, err := call(sock, "svctool.start", map[string]string{"pidfile": pf}); err != nil {
				t.Fatalf("start %d: %v", i, err)
			}
			pid, _ := readPid(t, pf)
			srvs = append(srvs, pid)
		}
		t0 := time.Now()
		_ = a.Process.Signal(syscall.SIGTERM)
		_, _ = a.Process.Wait()
		if d := time.Since(t0); d > 5*time.Second {
			t.Errorf("SIGTERM with 3 services took %s", d)
		}
		for i, p := range srvs {
			if procAlive(p) {
				t.Errorf("server %d alive after the adapter's SIGTERM exit", p)
			}
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("m%d.pid.term", i))); err != nil {
				t.Errorf("server %d was not stopped with SIGTERM", p)
			}
		}
		if left := groupMembers(apid); len(left) > 0 {
			t.Errorf("process group %d not empty after SIGTERM: %v", apid, left)
		}
	})

	// stop_signal: a service declared with SIGINT (PostgreSQL's fast
	// shutdown; its SIGTERM is a "smart" one that waits for every client) gets
	// SIGINT, not SIGTERM, when the adapter stops.
	t.Run("stop-signal", func(t *testing.T) {
		dir := shortDir(t)
		sock := filepath.Join(dir, "app.sock")
		a := spawn(t, sock)
		apid := a.Process.Pid
		pf := filepath.Join(dir, "i.pid")
		if _, err := call(sock, "svctool.startint", map[string]string{"pidfile": pf}); err != nil {
			t.Fatalf("startint: %v", err)
		}
		pid, _ := readPid(t, pf)
		_ = a.Process.Signal(syscall.SIGTERM)
		st, _ := a.Process.Wait()
		if st == nil || !st.Success() {
			t.Errorf("adapter exit after SIGTERM = %v, want 0", st)
		}
		if procAlive(pid) {
			t.Errorf("server %d alive after the adapter's SIGTERM exit", pid)
		}
		if _, err := os.Stat(pf + ".int"); err != nil {
			t.Errorf("server was not stopped with its stop_signal SIGINT (no i.pid.int)")
		}
		if _, err := os.Stat(pf + ".term"); err == nil {
			t.Errorf("server got SIGTERM although its stop_signal is SIGINT")
		}
		if left := groupMembers(apid); len(left) > 0 {
			t.Errorf("process group %d not empty after SIGTERM: %v", apid, left)
		}
	})
}
