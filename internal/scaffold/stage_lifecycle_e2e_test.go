//go:build !windows

package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestStageLifecycleE2E covers first-start asset staging for a cli app that
// ships its binary (io.pilot.miren 0.1.0 stages a 40-60 MB CLI tarball):
//
//   - The socket must appear, and help must answer, while the binary is still
//     downloading. The supervisor gives a new app 3s to create its socket and
//     otherwise never marks it ready, so a download that ran before the socket
//     (6-25 s for miren) left the app answering "app not ready" until its next
//     respawn.
//   - A stop during the download is a clean exit that removes the partial file;
//     a SIGKILLed download's leftover is swept by the next start. Neither may
//     land in the shared TMPDIR (miren stranded a pilot-asset-* there per
//     interrupted start).
//   - A failed download (no network on first start) is not fatal: help keeps
//     answering, a call reports the failure, and a later call retries.
func TestStageLifecycleE2E(t *testing.T) {
	bin, proj := buildCLIAdapter(t, cliAssetsSpec)
	manifest, err := os.ReadFile(filepath.Join(proj, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	// A ~2 MB "binary". Served at once; or the first 64 KiB, then nothing until
	// release is closed (mode "stall") or until the client goes away (mode
	// "hang"); or failing with HTTP 500 while fail is set.
	body := []byte("#!/bin/sh\necho toolx 1.0\n" + strings.Repeat("#"+strings.Repeat("x", 1022)+"\n", 2048))
	sum := sha256.Sum256(body)
	var (
		fail     atomic.Bool
		releaseM sync.Mutex
		release  = make(chan struct{})
	)
	releaseSlow := func() {
		releaseM.Lock()
		defer releaseM.Unlock()
		select {
		case <-release:
		default:
			close(release)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "registry down", http.StatusInternalServerError)
			return
		}
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write(body[:64<<10])
		w.(http.Flusher).Flush()
		wait := release
		if mode == "hang" {
			wait = nil // blocks until the client disconnects
		}
		select {
		case <-r.Context().Done():
			return
		case <-wait:
		}
		_, _ = w.Write(body[64<<10:])
	}))
	defer srv.Close()
	defer releaseSlow()

	type env struct{ app, tmp, sock string }
	// install.json is read at runtime, so point this host's asset at the local
	// server (the generated spec's registry URLs are placeholders).
	writeSpec := func(t *testing.T, e env, mode string) {
		t.Helper()
		url := srv.URL + "/toolx"
		if mode != "" {
			url += "?mode=" + mode
		}
		spec := map[string]any{"schema": 1, "app": "io.pilot.toolx", "version": "0.2.0", "command": "toolx",
			"assets": []map[string]any{{"role": "binary", "os": runtime.GOOS, "arch": runtime.GOARCH,
				"url": url, "sha256": hex.EncodeToString(sum[:]), "exec_path": "bin/toolx", "order": 1}}}
		raw, _ := json.Marshal(spec)
		if err := os.WriteFile(filepath.Join(e.app, "install.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T, mode string) env {
		t.Helper()
		dir := shortTempDir(t, "stg")
		e := env{app: filepath.Join(dir, "app"), tmp: filepath.Join(dir, "tmp"), sock: filepath.Join(dir, "app", "app.sock")}
		for _, d := range []string{e.app, e.tmp} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(e.app, "manifest.json"), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		writeSpec(t, e, mode)
		return e
	}
	start := func(t *testing.T, e env) *exec.Cmd {
		t.Helper()
		a := exec.Command(bin, "--socket", e.sock, "--manifest", filepath.Join(e.app, "manifest.json"))
		a.Env = append(os.Environ(), "TMPDIR="+e.tmp)
		a.Stderr = os.Stderr
		a.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // like the supervisor
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Process.Kill(); _, _ = a.Process.Wait() })
		return a
	}
	waitPartial := func(t *testing.T, e env) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			m, _ := filepath.Glob(filepath.Join(e.app, ".staged", "tmp", "pilot-asset-*"))
			for _, f := range m {
				if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
					return
				}
			}
		}
		t.Fatal("download never started")
	}
	noTmpLeft := func(t *testing.T, e env) {
		t.Helper()
		if ents, _ := os.ReadDir(e.tmp); len(ents) > 0 {
			t.Errorf("TMPDIR not empty: %v", ents)
		}
	}
	version := func(t *testing.T, e env) (string, error) {
		t.Helper()
		out, err := ipcCall(e.sock, "toolx.version", `{}`, 30*time.Second)
		return string(out), err
	}

	t.Run("socket and help are up while the binary is still downloading", func(t *testing.T) {
		e := setup(t, "stall")
		started := time.Now()
		start(t, e)
		// The supervisor's readiness window is 3s; the download is stalled.
		waitForSocket(t, e.sock, 2500*time.Millisecond)
		if _, err := ipcCall(e.sock, "toolx.help", `{}`, 5*time.Second); err != nil {
			t.Fatalf("help while staging: %v", err)
		}
		t.Logf("socket + help answered %s after start, download still stalled", time.Since(started).Round(time.Millisecond))
		waitPartial(t, e)

		// A call that needs the binary waits for it, then runs it.
		type res struct {
			out string
			err error
		}
		got := make(chan res, 1)
		go func() { o, err := version(t, e); got <- res{o, err} }()
		select {
		case r := <-got:
			t.Fatalf("call returned before the binary was installed: %q %v", r.out, r.err)
		case <-time.After(300 * time.Millisecond):
		}
		releaseSlow()
		r := <-got
		if r.err != nil || !strings.Contains(r.out, "toolx 1.0") {
			t.Fatalf("call after staging: out=%q err=%v", r.out, r.err)
		}
		if _, err := os.Stat(filepath.Join(e.app, ".staged", "tmp")); !os.IsNotExist(err) {
			t.Errorf("$APP/.staged/tmp left after staging: %v", err)
		}
		noTmpLeft(t, e)
	})

	t.Run("SIGTERM mid-download exits cleanly and removes the partial", func(t *testing.T) {
		e := setup(t, "hang")
		a := start(t, e)
		waitPartial(t, e)
		_ = a.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- a.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("adapter stopped mid-staging: %v (want a clean exit)", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("adapter did not exit within 5s of SIGTERM while staging")
		}
		if _, err := os.Stat(filepath.Join(e.app, ".staged", "tmp")); !os.IsNotExist(err) {
			t.Errorf("$APP/.staged/tmp left behind: %v", err)
		}
		if _, err := os.Stat(e.sock); !os.IsNotExist(err) {
			t.Errorf("socket left behind: %v", err)
		}
		noTmpLeft(t, e)
	})

	t.Run("SIGKILL mid-download: next start sweeps the leftover", func(t *testing.T) {
		e := setup(t, "hang")
		a := start(t, e)
		waitPartial(t, e)
		_ = a.Process.Kill()
		_, _ = a.Process.Wait()
		noTmpLeft(t, e)
		writeSpec(t, e, "")
		// A SIGKILLed adapter leaves app.sock; the supervisor removes a stale
		// socket before it spawns the next instance.
		_ = os.Remove(e.sock)
		start(t, e)
		waitForSocket(t, e.sock, 10*time.Second)
		if out, err := version(t, e); err != nil || !strings.Contains(out, "toolx 1.0") {
			t.Fatalf("call after restart: out=%q err=%v", out, err)
		}
		if _, err := os.Stat(filepath.Join(e.app, ".staged", "tmp")); !os.IsNotExist(err) {
			t.Errorf("$APP/.staged/tmp not swept by the next start: %v", err)
		}
		noTmpLeft(t, e)
	})

	t.Run("a failed download keeps serving and a later call retries", func(t *testing.T) {
		fail.Store(true)
		defer fail.Store(false)
		e := setup(t, "")
		a := start(t, e)
		waitForSocket(t, e.sock, 2500*time.Millisecond)
		// The first attempt fails; the call reports it as an error.
		var err error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if _, err = version(t, e); err != nil && strings.Contains(err.Error(), "HTTP 500") {
				break
			}
		}
		if err == nil || !strings.Contains(err.Error(), "install assets") || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("call with the registry down: err=%v, want an install-assets error naming HTTP 500", err)
		}
		if _, err := ipcCall(e.sock, "toolx.help", `{}`, 5*time.Second); err != nil {
			t.Fatalf("help after a failed download: %v", err)
		}
		if err := syscall.Kill(a.Process.Pid, 0); err != nil {
			t.Fatalf("adapter exited after a failed download: %v", err)
		}
		// Registry back: a call after the retry delay installs and runs it.
		fail.Store(false)
		var out string
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if out, err = version(t, e); err == nil {
				break
			}
		}
		if err != nil || !strings.Contains(out, "toolx 1.0") {
			t.Fatalf("call once the registry is back: out=%q err=%v", out, err)
		}
		noTmpLeft(t, e)
	})
}
