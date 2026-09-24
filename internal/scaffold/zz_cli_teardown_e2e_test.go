//go:build !windows

package scaffold

import (
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

// fakeSmolEnv switches the test binary into a stand-in for the smolvm CLI:
// TestHelperFakeSmol. fakeSmolStateEnv names its state directory.
const (
	fakeSmolEnv      = "SCAFFOLD_TEST_FAKE_SMOL"
	fakeSmolStateEnv = "SCAFFOLD_TEST_FAKE_SMOL_STATE"
)

// TestHelperFakeSmol is not a test: it is the fake `smolvm` of
// TestCLITeardownE2E and skips unless fakeSmolEnv is set. It models the parts
// of smolvm 1.2.0 that matter for the VM's lifetime:
//
//   - `machine run` / `machine start` spawn the VM (`_boot-vm <name>`) in its
//     own process group, as smolvm does. With SMOLVM_BOOT_BINARY set, the VM
//     watches its parent and exits when the CLI is gone (upstream's watchdog,
//     manager.rs); without it, it runs until `machine stop`.
//   - `machine run` without -d waits for the VM like the real foreground run,
//     and tears it down only on SIGINT (upstream's SigintGuard): SIGTERM or
//     SIGKILL of the CLI orphans the VM.
//   - `machine start` and `machine run -d` return at once and leave the VM up.
//   - `machine stop` kills the named VM and appends its name to <state>/stops.
//   - a VM named "fail" fails to start (exit 1) and starts nothing.
func TestHelperFakeSmol(t *testing.T) {
	if os.Getenv(fakeSmolEnv) == "" {
		t.Skip("helper process for TestCLITeardownE2E")
	}
	state := os.Getenv(fakeSmolStateEnv)
	args := flag.Args()
	name := func() string {
		for i, a := range args {
			if a == "--" {
				break
			}
			if (a == "--name" || a == "-n") && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, "--name=") {
				return strings.TrimPrefix(a, "--name=")
			}
		}
		return "default"
	}
	has := func(flags ...string) bool {
		for _, a := range args {
			if a == "--" {
				return false
			}
			for _, f := range flags {
				if a == f {
					return true
				}
			}
		}
		return false
	}
	pidFile := func(n string) string { return filepath.Join(state, "vm-"+n+".pid") }
	if len(args) >= 2 && args[0] == "_boot-vm" {
		n := args[1]
		parent := os.Getppid()
		_ = os.WriteFile(pidFile(n), []byte(strconv.Itoa(os.Getpid())), 0o644)
		signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
		if os.Getenv("SMOLVM_BOOT_WATCH_PARENT") == "1" {
			for os.Getppid() == parent {
				time.Sleep(20 * time.Millisecond)
			}
			os.Exit(0)
		}
		for {
			time.Sleep(time.Hour) // not select{}: the runtime would call that a deadlock
		}
	}
	if len(args) < 2 || args[0] != "machine" {
		fmt.Println(`{"version":"fake"}`)
		os.Exit(0)
	}
	n := name()
	switch args[1] {
	case "run", "start":
		detached := args[1] == "start" || has("-d", "--detach")
		_ = os.WriteFile(filepath.Join(state, "env-"+n), []byte(os.Getenv("SMOLVM_BOOT_BINARY")), 0o644)
		if n == "fail" {
			fmt.Fprintln(os.Stderr, "Error: machine 'fail' could not start")
			os.Exit(1)
		}
		vm := exec.Command(os.Args[0], "-test.run=^TestHelperFakeSmol$", "--", "_boot-vm", n)
		watch := "0"
		if os.Getenv("SMOLVM_BOOT_BINARY") != "" {
			watch = "1"
		}
		vm.Env = append(os.Environ(), "SMOLVM_BOOT_WATCH_PARENT="+watch)
		vm.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := vm.Start(); err != nil {
			os.Exit(2)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(pidFile(n)); err == nil {
				break
			}
		}
		if detached {
			fmt.Println("started " + n)
			os.Exit(0)
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT)
		<-sig
		_ = vm.Process.Kill()
		os.Exit(130)
	case "stop", "delete":
		b, err := os.ReadFile(pidFile(n))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: machine '%s' is not running\n", n)
			os.Exit(1)
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = os.Remove(pidFile(n))
		f, _ := os.OpenFile(filepath.Join(state, "stops"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, n)
		_ = f.Close()
		os.Exit(0)
	}
	os.Exit(0)
}

// TestCLITeardownE2E: nothing a cli call starts may outlive the adapter, even
// when the CLI hands it off to a process outside the adapter's process group.
// Found with io.pilot.smol 1.2.0 on darwin/arm64 with real smolvm VMs:
//
//   - an ephemeral `machine run` VM was orphaned (ppid 1, own group) when the
//     CLI got SIGTERM (app stop, process-group stop) or SIGKILL (the 60 s call
//     deadline): 2/2, 2/2 and 1/1 runs. smolvm tears it down only on SIGINT.
//     Fixed with cli.env_rules: SMOLVM_BOOT_BINARY arms smolvm's own
//     parent-death watchdog in the VM for a foreground run.
//   - VMs from `machine start` and `machine run -d` kept running after the
//     app's SIGTERM and after the supervisor's SIGKILL of the app. Fixed with
//     cli.teardown: the adapter stops them with `machine stop --name <name>` on
//     a clean stop, and its child guard does it when the adapter is killed.
func TestCLITeardownE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	toolDir := filepath.Join(root, "smolvm-9.9.9")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(toolDir, "smolvm")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec env " + fakeSmolEnv + "=1 " + fakeSmolStateEnv + "='" + state + "' '" + os.Args[0] + "' -test.run='^TestHelperFakeSmol$' -- \"$@\"\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := parseSpec(t, `
id: io.pilot.fakesmol
app_version: 0.1.0
description: "Fronts a fake smolvm."
namespace: fakesmol
backend:
  type: cli
  command: ["`+tool+`"]
methods:
  - name: fakesmol.exec
    summary: "Passthrough."
    timeout: 4s
    cli:
      passthrough: true
      env_rules:
        - argv_prefix: [machine, run]
          unless_flags: [-d, --detach]
          env: {SMOLVM_BOOT_BINARY: "${command_dir}/smolvm-bin"}
      teardown:
        - argv_prefix: [machine, start]
          name_flags: [--name, -n]
          default_name: default
          run: [machine, stop, --name, "${name}"]
          clear_prefixes: [[machine, stop], [machine, delete]]
        - argv_prefix: [machine, run]
          when_flags: [-d, --detach]
          name_flags: [--name, -n]
          default_name: default
          run: [machine, stop, --name, "${name}"]
          clear_prefixes: [[machine, stop], [machine, delete]]
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
	spawn := func(t *testing.T) (*exec.Cmd, string) {
		t.Helper()
		dir, err := os.MkdirTemp("", "td") // short: sun_path is ~104 bytes on darwin
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "app.sock")
		a := exec.Command(bin, "--socket", sock, "--manifest", manifest)
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		a.Stderr = os.Stderr
		if err := a.Start(); err != nil {
			t.Fatalf("start adapter: %v", err)
		}
		pid := a.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = a.Process.Wait() })
		waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil }, 10*time.Second, "adapter socket")
		return a, sock
	}
	call := func(sock string, args ...string) (map[string]any, error) {
		conn, err := net.DialTimeout("unix", sock, 3*time.Second)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		var out json.RawMessage
		if err := ipc.Call(conn, "fakesmol.exec", map[string]any{"args": args}, &out); err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return m, nil
	}
	vmPid := func(t *testing.T, name string) int {
		t.Helper()
		var pid int
		waitFor(t, func() bool {
			b, err := os.ReadFile(filepath.Join(state, "vm-"+name+".pid"))
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			return err == nil && pid > 0
		}, 10*time.Second, "VM "+name+" to start")
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		return pid
	}
	stops := func() map[string]int {
		b, _ := os.ReadFile(filepath.Join(state, "stops"))
		m := map[string]int{}
		for _, l := range strings.Fields(string(b)) {
			m[l]++
		}
		return m
	}
	reset := func() {
		_ = os.Remove(filepath.Join(state, "stops"))
	}
	envOf := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(state, "env-"+name))
		return string(b)
	}
	stopBy := map[string]func(a *exec.Cmd){
		"sigterm": func(a *exec.Cmd) { _ = a.Process.Signal(syscall.SIGTERM); _, _ = a.Process.Wait() },
		// The app-store supervisor's stop path through v1.0.3.
		"sigkill": func(a *exec.Cmd) { _ = a.Process.Kill(); _, _ = a.Process.Wait() },
		// A supervisor that signals the whole group.
		"group-sigterm": func(a *exec.Cmd) { _ = syscall.Kill(-a.Process.Pid, syscall.SIGTERM); _, _ = a.Process.Wait() },
	}

	// Ephemeral VM: however the adapter stops mid-run, the VM goes with it.
	for _, how := range []string{"sigterm", "sigkill", "group-sigterm"} {
		t.Run("ephemeral/"+how, func(t *testing.T) {
			reset()
			a, sock := spawn(t)
			name := "eph-" + how
			go func() {
				_, _ = call(sock, "machine", "run", "--name", name, "--image", "alpine", "--", "sleep", "-d", "30")
			}()
			vm := vmPid(t, name)
			if got, want := envOf(name), filepath.Join(toolDir, "smolvm-bin"); got != want {
				t.Errorf("SMOLVM_BOOT_BINARY for a foreground run = %q, want %q", got, want)
			}
			stopBy[how](a)
			waitFor(t, func() bool { return !procAlive(vm) }, 10*time.Second, "ephemeral VM to die after adapter "+how)
			if left := groupLeft(a.Process.Pid, 3*time.Second); len(left) > 0 {
				t.Errorf("adapter group not empty after %s: %v", how, left)
			}
		})
	}

	// The call deadline (timeout: 4s) stops the CLI with SIGTERM, then SIGKILL:
	// the VM must not survive it.
	t.Run("ephemeral/deadline", func(t *testing.T) {
		reset()
		_, sock := spawn(t)
		done := make(chan error, 1)
		go func() {
			_, err := call(sock, "machine", "run", "--name", "eph-deadline", "--", "sleep", "30")
			done <- err
		}()
		vm := vmPid(t, "eph-deadline")
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "deadline") {
				t.Errorf("run past the deadline: err = %v, want a deadline error", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("call did not end at its deadline")
		}
		waitFor(t, func() bool { return !procAlive(vm) }, 10*time.Second, "VM to die after the call deadline")
	})

	// Persistent VMs: stopped with `machine stop --name <name>` on SIGTERM, by
	// the child guard on SIGKILL, and after a supervisor group SIGTERM.
	for _, how := range []string{"sigterm", "sigkill", "group-sigterm"} {
		t.Run("persistent/"+how, func(t *testing.T) {
			reset()
			a, sock := spawn(t)
			starts := [][]string{
				{"machine", "start", "--name", "p1-" + how},
				{"machine", "run", "-d", "--name=p2-" + how, "--image", "alpine"},
				{"machine", "start"}, // the default VM
			}
			var vms []int
			for _, s := range starts {
				res, err := call(sock, s...)
				if err != nil || res["exit"] != float64(0) {
					t.Fatalf("%v = %v, %v", s, res, err)
				}
			}
			for _, n := range []string{"p1-" + how, "p2-" + how, "default"} {
				vms = append(vms, vmPid(t, n))
				if env := envOf(n); env != "" {
					t.Errorf("VM %s: SMOLVM_BOOT_BINARY=%q on a persistent start; its VM would die with the CLI", n, env)
				}
			}
			// Long past the CLI's exit and the guard's idle retirement: the
			// VMs are still up (persistent), held only by the teardown records.
			time.Sleep(1500 * time.Millisecond)
			for _, p := range vms {
				if !procAlive(p) {
					t.Fatalf("persistent VM %d died before the adapter stopped", p)
				}
			}
			stopBy[how](a)
			for _, p := range vms {
				waitFor(t, func() bool { return !procAlive(p) }, 10*time.Second, fmt.Sprintf("VM %d to be stopped after adapter %s", p, how))
			}
			names := []string{"p1-" + how, "p2-" + how, "default"}
			// The fake stop logs its name after it has killed the VM.
			waitFor(t, func() bool {
				got := stops()
				return got[names[0]] > 0 && got[names[1]] > 0 && got[names[2]] > 0
			}, 5*time.Second, "every machine stop to finish")
			got := stops()
			for _, n := range names {
				if got[n] != 1 {
					t.Errorf("machine stop --name %s ran %d times, want 1 (stops: %v)", n, got[n], got)
				}
			}
			if left := groupLeft(a.Process.Pid, 3*time.Second); len(left) > 0 {
				t.Errorf("adapter group not empty after %s: %v", how, left)
			}
		})
	}

	// An explicit stop (any spelling of the name) closes the record, and a
	// start that fails opens none: nothing is stopped twice or needlessly.
	t.Run("records", func(t *testing.T) {
		reset()
		a, sock := spawn(t)
		if _, err := call(sock, "machine", "start", "-n", "c1"); err != nil {
			t.Fatal(err)
		}
		vm := vmPid(t, "c1")
		if res, err := call(sock, "machine", "stop", "--name=c1"); err != nil || res["exit"] != float64(0) {
			t.Fatalf("machine stop = %v, %v", res, err)
		}
		waitFor(t, func() bool { return !procAlive(vm) }, 5*time.Second, "c1 to stop")
		if res, err := call(sock, "machine", "start", "--name", "fail"); err != nil || res["exit"] != float64(1) {
			t.Fatalf("failing start = %v, %v", res, err)
		}
		// A read-only command opens nothing.
		if _, err := call(sock, "machine", "ls"); err != nil {
			t.Fatal(err)
		}
		_ = a.Process.Signal(syscall.SIGTERM)
		_, _ = a.Process.Wait()
		got := stops()
		if got["c1"] != 1 || got["fail"] != 0 || len(got) != 1 {
			t.Errorf("stops = %v, want only c1 once (the explicit stop)", got)
		}
	})
}

// TestValidateCLILifecycle: env_rules/teardown are checked at generation time.
func TestValidateCLILifecycle(t *testing.T) {
	base := func(cli string) string {
		return `
id: io.pilot.x
app_version: 0.1.0
description: "x"
namespace: x
backend: {type: cli, command: ["x"]}
methods:
  - name: x.exec
    summary: "x"
    cli:
` + cli
	}
	cases := []struct {
		name, cli, want string
	}{
		{"ok", `      passthrough: true
      env_rules: [{argv_prefix: [a], unless_flags: [-d], env: {K: "${command_dir}/b"}}]
      teardown: [{argv_prefix: [a], name_flags: [--name], run: [stop, "${name}"], clear_prefixes: [[stop]]}]
`, ""},
		{"not passthrough", `      args: [a]
      teardown: [{argv_prefix: [a], default_name: d, run: [stop]}]
`, "only apply to a passthrough route"},
		{"bad env placeholder", `      passthrough: true
      env_rules: [{argv_prefix: [a], env: {K: "${home}"}}]
`, "only ${command_dir}"},
		{"bad env name", `      passthrough: true
      env_rules: [{argv_prefix: [a], env: {"1K": "v"}}]
`, "not a valid variable name"},
		{"empty prefix", `      passthrough: true
      teardown: [{argv_prefix: [], default_name: d, run: [stop]}]
`, "argv_prefix must not be empty"},
		{"name without source", `      passthrough: true
      teardown: [{argv_prefix: [a], run: [stop, "${name}"]}]
`, "no name_flags or default_name"},
		{"bad flag", `      passthrough: true
      teardown: [{argv_prefix: [a], when_flags: [detach], default_name: d, run: [stop]}]
`, "must be a flag"},
		{"no run", `      passthrough: true
      teardown: [{argv_prefix: [a], default_name: d}]
`, "run (the argv that stops"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateSpec(t, base(tc.cli))
			if tc.want == "" {
				if errs != "" {
					t.Fatalf("want valid, got %s", errs)
				}
				return
			}
			if !strings.Contains(errs, tc.want) {
				t.Fatalf("errs = %q, want it to mention %q", errs, tc.want)
			}
		})
	}
}
