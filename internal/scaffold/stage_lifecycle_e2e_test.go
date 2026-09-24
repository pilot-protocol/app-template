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
	"syscall"
	"testing"
	"time"
)

// TestStageInterruptedLeavesNothingE2E: stopping an adapter while it is still
// downloading its native binary on first start must not leave the partial
// download behind in the shared TMPDIR (io.pilot.miren: a 40-60 MB
// pilot-asset-* per interrupted start), SIGTERM must be handled (clean exit,
// partial removed), and a SIGKILLed start's leftover must be swept by the next
// start.
func TestStageInterruptedLeavesNothingE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	cfg := parseSpec(t, cliAssetsSpec)
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

	// A ~2 MB "binary", served either slowly (64 KiB per 100 ms) or at once.
	body := []byte("#!/bin/sh\necho toolx 1.0\n" + strings.Repeat("#"+strings.Repeat("x", 1022)+"\n", 2048))
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") == "" {
			_, _ = w.Write(body)
			return
		}
		for off := 0; off < len(body); off += 64 << 10 {
			end := min(off+64<<10, len(body))
			if _, err := w.Write(body[off:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	type env struct{ app, tmp, sock string }
	// install.json is read at runtime, so point this host's asset at the local
	// server (the generated spec's R2 URLs are placeholders).
	writeSpec := func(t *testing.T, e env, slow bool) {
		t.Helper()
		url := srv.URL + "/toolx"
		if slow {
			url += "?slow=1"
		}
		spec := map[string]any{"schema": 1, "app": "io.pilot.toolx", "version": "0.2.0", "command": "toolx",
			"assets": []map[string]any{{"name": "toolx", "role": "binary", "os": runtime.GOOS, "arch": runtime.GOARCH,
				"url": url, "sha256": hex.EncodeToString(sum[:]), "exec_path": "bin/toolx", "order": 1}}}
		raw, _ := json.Marshal(spec)
		if err := os.WriteFile(filepath.Join(e.app, "install.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T, slow bool) env {
		t.Helper()
		dir, err := os.MkdirTemp("", "stg") // short: sun_path
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		e := env{app: filepath.Join(dir, "app"), tmp: filepath.Join(dir, "tmp"), sock: filepath.Join(dir, "app", "app.sock")}
		for _, d := range []string{e.app, e.tmp} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		mf, _ := os.ReadFile(filepath.Join(proj, "manifest.json"))
		_ = os.WriteFile(filepath.Join(e.app, "manifest.json"), mf, 0o644)
		writeSpec(t, e, slow)
		return e
	}
	start := func(t *testing.T, e env) *exec.Cmd {
		t.Helper()
		a := exec.Command(bin, "--socket", e.sock, "--manifest", filepath.Join(e.app, "manifest.json"))
		a.Env = append(os.Environ(), "TMPDIR="+e.tmp)
		a.Stderr = os.Stderr
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
			tm, _ := filepath.Glob(filepath.Join(e.tmp, "pilot-asset-*"))
			for _, f := range append(m, tm...) {
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

	t.Run("SIGTERM mid-download exits cleanly and removes the partial", func(t *testing.T) {
		e := setup(t, true)
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
		noTmpLeft(t, e)
	})

	t.Run("SIGKILL mid-download: next start sweeps the leftover", func(t *testing.T) {
		e := setup(t, true)
		a := start(t, e)
		waitPartial(t, e)
		_ = a.Process.Kill()
		_, _ = a.Process.Wait()
		noTmpLeft(t, e)
		writeSpec(t, e, false)
		b := start(t, e)
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(e.sock); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("second start never listened")
			}
		}
		if _, err := os.Stat(filepath.Join(e.app, ".staged", "tmp")); !os.IsNotExist(err) {
			t.Errorf("$APP/.staged/tmp not swept by the next start: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.app, "bin", "toolx")); err != nil {
			t.Errorf("asset not staged: %v", err)
		}
		noTmpLeft(t, e)
		_ = b.Process.Signal(syscall.SIGTERM)
		_, _ = b.Process.Wait()
	})
}
