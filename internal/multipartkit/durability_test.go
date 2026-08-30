package multipartkit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The daemon supervises adapters and respawns them, so "the process restarted"
// is an ordinary event rather than a disaster case. These tests pin what has to
// survive that and what must not.

// TestSealedBlobSurvivesRestart: an upload staged and finalized before a respawn
// is still sendable afterwards. Keeping the only record of a finished upload in
// a map means an agent that staged 20 MiB has to stage it all over again for
// reasons it cannot see.
func TestSealedBlobSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	content := randN(t, 3<<20)

	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	id := stage(t, s1, "nda.docx", "application/pdf", content)

	// A new Store over the same root is what the respawned adapter gets.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	f, meta, err := s2.Open(id)
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(content) {
		t.Fatal("blob bytes changed across the restart")
	}
	if meta.FileName != "nda.docx" || meta.ContentType != "application/pdf" {
		t.Errorf("metadata lost across restart: %+v", meta)
	}
}

// TestInFlightPartSweptOnRestart: a half-written .part cannot be finished — the
// rolling hash and the chunk cursor died with the process. Leaving it on disk
// grows $APP forever and invites a later resume that would splice a gap into
// the middle of a document.
func TestInFlightPartSweptOnRestart(t *testing.T) {
	dir := t.TempDir()
	content := randN(t, 4096)

	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	m, err := s1.Begin("half.docx", "", int64(len(content)), sha(content))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := s1.Append(m.ID, 0, content[:2048]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, m.ID+".part")); err != nil {
		t.Fatalf("test setup: .part missing before restart: %v", err)
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, m.ID+".part")); !os.IsNotExist(err) {
		t.Errorf("orphan .part survived the restart (err = %v)", err)
	}
	if _, err := s2.Append(m.ID, 1, content[2048:]); !errors.Is(err, ErrNotFound) {
		t.Errorf("resuming a swept upload: err = %v, want ErrNotFound", err)
	}
	if _, _, err := s2.Open(m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening a swept upload: err = %v, want ErrNotFound", err)
	}
}

// TestOpenBeforeFinalizeRefused: a staged-but-unsealed blob must not be
// sendable — its bytes have not been checked against the declared sha yet.
func TestOpenBeforeFinalizeRefused(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 4096)
	m, _ := s.Begin("x.docx", "", int64(len(content)), sha(content))
	if _, err := s.Append(m.ID, 0, content); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, _, err := s.Open(m.ID); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Open before Finalize: err = %v, want ErrIncomplete", err)
	}
}

// TestAppendAfterFinalizeIsSealed: once sealed, the bytes behind a blob id are
// fixed. Appending must report that clearly rather than as "unknown id", or a
// caller cannot tell a finished upload from a typo.
func TestAppendAfterFinalizeIsSealed(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 1024)
	id := stage(t, s, "x.docx", "", content)
	if _, err := s.Append(id, 1, []byte("more")); !errors.Is(err, ErrSealed) {
		t.Fatalf("Append after Finalize: err = %v, want ErrSealed", err)
	}
}

// TestFinalizeIsIdempotent: a retried finalize (an agent that lost the reply)
// must not fail.
func TestFinalizeIsIdempotent(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 1024)
	id := stage(t, s, "x.docx", "", content)
	m, err := s.Finalize(id)
	if err != nil {
		t.Fatalf("second Finalize: %v", err)
	}
	if m.ID != id {
		t.Errorf("Finalize returned id %q, want %q", m.ID, id)
	}
}

// TestGCReclaimsSealedBlobsOnDisk: GC used to walk only the in-memory map, so
// blobs sealed before a restart were never reclaimed and $APP grew without
// bound.
func TestGCReclaimsSealedBlobsOnDisk(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	id := stage(t, s1, "x.docx", "", randN(t, 2048))

	// Age the sealed blob past any TTL.
	old := time.Now().Add(-2 * time.Hour)
	for _, suffix := range []string{".blob", ".meta"} {
		if err := os.Chtimes(filepath.Join(dir, id+suffix), old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	s2, _ := NewStore(dir) // restart: nothing in the map
	s2.SetLimits(0, time.Minute)
	if n := s2.GC(); n != 1 {
		t.Fatalf("GC reclaimed %d, want 1", n)
	}
	if _, _, err := s2.Open(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob still open after GC: %v", err)
	}
	left, _ := os.ReadDir(dir)
	for _, e := range left {
		if strings.HasPrefix(e.Name(), id) {
			t.Errorf("GC left %s behind", e.Name())
		}
	}
}

// TestGCKeepsFreshBlobs: reclaiming aggressively would delete a document the
// agent is about to send.
func TestGCKeepsFreshBlobs(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	id := stage(t, s, "x.docx", "", randN(t, 1024))
	if n := s.GC(); n != 0 {
		t.Fatalf("GC reclaimed %d fresh blobs, want 0", n)
	}
	f, _, err := s.Open(id)
	if err != nil {
		t.Fatalf("fresh blob was reclaimed: %v", err)
	}
	f.Close()
}

// TestConcurrentAppendAndAbort: Append writes to a file that Abort closes, so
// the two must be serialised. Run under -race this catches the window where
// Append looked the blob up, released the lock, and then wrote.
func TestConcurrentAppendAndAbort(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		content := randN(t, 8192)
		m, err := s.Begin("x.bin", "", int64(len(content)), sha(content))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		wg.Add(2)
		go func(id string, data []byte) {
			defer wg.Done()
			// Errors are expected and fine here — a torn write or a crash is not.
			_, _ = s.Append(id, 0, data[:4096])
			_, _ = s.Append(id, 1, data[4096:])
			_, _ = s.Finalize(id)
		}(m.ID, content)
		go func(id string) {
			defer wg.Done()
			_ = s.Abort(id)
		}(m.ID)
	}
	wg.Wait()
}

// TestConcurrentBeginsDoNotCollide: ids are minted under contention too.
func TestConcurrentBeginsDoNotCollide(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	const n = 64
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := s.Begin("x", "", 8, sha([]byte("12345678")))
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			ids <- m.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate blob id under contention: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("minted %d ids, want %d", len(seen), n)
	}
}

// TestMetaWithoutBlobIsNotOpenable: a .meta whose bytes are missing or the
// wrong length must not present as a usable upload. Sealed state is two files
// and both have to agree.
func TestMetaWithoutBlobIsNotOpenable(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	id := stage(t, s, "x.docx", "", randN(t, 2048))

	if err := os.Remove(filepath.Join(dir, id+".blob")); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	s2, _ := NewStore(dir)
	if _, _, err := s2.Open(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("meta without blob: err = %v, want ErrNotFound", err)
	}

	// And a blob truncated behind the metadata's back is not usable either.
	id2 := stage(t, s, "y.docx", "", randN(t, 2048))
	if err := os.Truncate(filepath.Join(dir, id2+".blob"), 10); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	s3, _ := NewStore(dir)
	if _, _, err := s3.Open(id2); err == nil {
		t.Error("a truncated blob opened successfully")
	}
}
