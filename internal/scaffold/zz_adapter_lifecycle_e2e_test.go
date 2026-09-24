//go:build !windows

package scaffold

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// lifecycleSpec is a byo http app with one fast and one slow method. The slow
// backend route answers only when its request is cancelled, so a test can see
// whether the adapter gives up on a call its caller has abandoned.
const lifecycleSpec = `
id: io.pilot.lifecyclex
app_version: 0.1.0
description: "App exercising the adapter process lifecycle."
namespace: lifecyclex
backend:
  base_url: https://placeholder.invalid
methods:
  - name: lifecyclex.fast
    summary: "Answers at once."
    http: { verb: GET, path: "/fast" }
  - name: lifecyclex.slow
    summary: "Answers only when cancelled."
    http: { verb: POST, path: "/slow" }
`

// lifecycleAdapter is a built lifecyclex adapter plus the backend it talks to.
type lifecycleAdapter struct {
	bin       string
	backend   *httptest.Server
	slowOpen  atomic.Int64 // /slow requests still waiting on the backend
	cancelled atomic.Int64 // /slow requests the adapter abandoned
}

func buildLifecycleAdapter(t *testing.T) *lifecycleAdapter {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	la := &lifecycleAdapter{}
	la.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		// Drain the body first: net/http only watches an HTTP/1 connection
		// for the client going away once the request body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		la.slowOpen.Add(1)
		defer la.slowOpen.Add(-1)
		select {
		case <-r.Context().Done():
			la.cancelled.Add(1)
		case <-time.After(30 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"late":true}`))
		}
	}))
	t.Cleanup(la.backend.Close)

	root := t.TempDir()
	cfg := parseSpec(t, lifecycleSpec)
	proj := filepath.Join(root, "proj")
	if _, err := Generate(cfg, proj); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		_ = os.WriteFile(filepath.Join(proj, "go.sum"), sum, 0o644)
	}
	la.bin = filepath.Join(root, "adapter")
	build := exec.Command("go", "build", "-o", la.bin, "./cmd/"+cfg.BinaryName)
	build.Dir = proj
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v\n%s", err, out)
	}
	return la
}

// sockPath returns a socket path short enough for sun_path (~104 on darwin).
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

func (la *lifecycleAdapter) env() []string {
	return append(os.Environ(), "LIFECYCLEX_BACKEND_URL="+la.backend.URL)
}

// start runs the adapter as a direct child of the test process.
func (la *lifecycleAdapter) start(t *testing.T, sock string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(la.bin, "--socket", sock)
	cmd.Env = la.env()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

func waitSocket(t *testing.T, sock string, notInode uint64) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if fi, err := os.Stat(sock); err == nil && inode(fi) != notInode {
			return
		}
	}
	t.Fatalf("socket %s never appeared", sock)
}

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func callFast(sock string) error {
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var out json.RawMessage
	return ipc.Call(conn, "lifecyclex.fast", json.RawMessage(`{}`), &out)
}

// sendSlow opens a connection and sends one lifecyclex.slow request without
// waiting for the reply; the caller decides when to hang up.
func sendSlow(t *testing.T, sock string, i int) net.Conn {
	t.Helper()
	conn, err := trySendSlow(sock, i)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func trySendSlow(sock string, i int) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %d: %w", i, err)
	}
	req := &ipc.Envelope{Type: ipc.EnvReq, ReqID: fmt.Sprintf("r%d", i), Method: "lifecyclex.slow", Payload: json.RawMessage(`{}`)}
	if err := ipc.WriteFrame(conn, req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write %d: %w", i, err)
	}
	return conn, nil
}

// syncBuffer is a bytes.Buffer safe to read while exec's copier writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func eventually(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// TestGeneratedAdapterLifecycleE2E runs a real generated adapter binary and
// checks it never outlives, or disturbs, what the daemon expects:
//
//   - it exits when the process that spawned it dies (macOS has no Pdeathsig);
//   - an old instance exiting leaves a newer instance's socket in place;
//   - a caller hanging up cancels the call's backend request;
//   - running out of descriptors is survived, not fatal.
func TestGeneratedAdapterLifecycleE2E(t *testing.T) {
	la := buildLifecycleAdapter(t)

	t.Run("exits when its parent dies", func(t *testing.T) {
		sock := sockPath(t)
		// A shell stands in for the daemon: it starts the adapter, prints its
		// pid, and is then SIGKILLed the way a crashed daemon disappears.
		parent := exec.Command("sh", "-c", `"$0" --socket "$1" & echo $!; exec sleep 60`, la.bin, sock)
		parent.Env = la.env()
		parent.Stderr = os.Stderr
		out, err := parent.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := parent.Start(); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil {
			t.Fatalf("read adapter pid: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("adapter pid %q: %v", line, err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		waitSocket(t, sock, 0)
		if err := callFast(sock); err != nil {
			t.Fatalf("call before parent death: %v", err)
		}

		_ = parent.Process.Kill()
		_, _ = parent.Process.Wait()
		if !eventually(3*time.Second, func() bool { return !alive(pid) }) {
			t.Fatalf("adapter pid %d still running 3s after its parent was killed (orphaned)", pid)
		}
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("socket %s left behind after the orphaned adapter exited (stat err=%v)", sock, err)
		}
	})

	t.Run("old instance exiting keeps the newer socket", func(t *testing.T) {
		sock := sockPath(t)
		old := la.start(t, sock)
		waitSocket(t, sock, 0)
		fi, err := os.Stat(sock)
		if err != nil {
			t.Fatal(err)
		}
		// A respawn next to an orphan: the new instance replaces app.sock.
		la.start(t, sock)
		waitSocket(t, sock, inode(fi))

		_ = old.Process.Signal(syscall.SIGTERM)
		_, _ = old.Process.Wait()
		if _, err := os.Stat(sock); err != nil {
			t.Fatalf("old instance's exit removed the new instance's socket: %v", err)
		}
		if err := callFast(sock); err != nil {
			t.Errorf("new instance unreachable after the old one exited: %v", err)
		}
	})

	t.Run("caller hang-up cancels the backend request", func(t *testing.T) {
		sock := sockPath(t)
		la.start(t, sock)
		waitSocket(t, sock, 0)
		before := la.cancelled.Load()
		const n = 5
		for i := 0; i < n; i++ {
			conn := sendSlow(t, sock, i)
			if !eventually(3*time.Second, func() bool { return la.slowOpen.Load() > 0 }) {
				t.Fatalf("call %d never reached the backend", i)
			}
			_ = conn.Close() // the caller gives up
			if !eventually(3*time.Second, func() bool { return la.cancelled.Load() == before+int64(i)+1 }) {
				t.Fatalf("call %d: backend request still open 3s after its caller hung up (cancelled=%d)", i, la.cancelled.Load()-before)
			}
		}
		if err := callFast(sock); err != nil {
			t.Errorf("adapter unhealthy after abandoned calls: %v", err)
		}
	})

	t.Run("survives running out of descriptors", func(t *testing.T) {
		sock := sockPath(t)
		// Few descriptors, so a handful of waiting callers exhaust them.
		cmd := exec.Command("sh", "-c", `ulimit -n 32 && exec "$0" --socket "$1"`, la.bin, sock)
		cmd.Env = la.env()
		var logs syncBuffer
		cmd.Stderr = io.MultiWriter(os.Stderr, &logs)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		waitSocket(t, sock, 0)

		var conns []net.Conn
		for i := 0; i < 40; i++ { // two descriptors each: more than 32 in total
			c, err := trySendSlow(sock, i)
			if err != nil {
				// A caller may be turned away here: when accept(2) fails with
				// EMFILE, macOS drops the pending connection (the peer sees
				// EPIPE/ECONNRESET) where Linux leaves it queued. Either way
				// the adapter itself must keep running.
				t.Logf("caller %d turned away while the adapter is out of descriptors: %v", i, err)
				continue
			}
			conns = append(conns, c)
		}
		time.Sleep(1500 * time.Millisecond)
		select {
		case err := <-exited:
			t.Fatalf("adapter exited while out of descriptors: %v", err)
		default:
		}
		if !strings.Contains(logs.String(), "too many open files") {
			t.Fatalf("adapter never ran out of descriptors, so this proves nothing; its log:\n%s", logs.String())
		}
		for _, c := range conns {
			_ = c.Close()
		}
		if !eventually(5*time.Second, func() bool { return callFast(sock) == nil }) {
			t.Fatalf("adapter did not recover after callers hung up: %v", callFast(sock))
		}
	})
}
