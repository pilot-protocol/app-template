package multipartkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	mrand "math/rand"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// randN builds test content from a seeded PRNG. Integrity here is checked by
// sha256, which only needs bytes that change when the content changes —
// crypto/rand by the megabyte buys nothing and costs seconds.
func randN(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	r := mrand.New(mrand.NewSource(int64(n) * 2654435761))
	_, _ = r.Read(b)
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// stage pushes content through the chunked API the way an agent would.
func stage(t *testing.T, s *Store, name, ct string, content []byte) string {
	t.Helper()
	m, err := s.Begin(name, ct, int64(len(content)), sha(content))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for seq, off := 0, 0; off < len(content); seq, off = seq+1, off+MaxChunkBytes {
		end := off + MaxChunkBytes
		if end > len(content) {
			end = len(content)
		}
		if _, err := s.Append(m.ID, seq, content[off:end]); err != nil {
			t.Fatalf("Append seq %d: %v", seq, err)
		}
	}
	if _, err := s.Finalize(m.ID); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return m.ID
}

// TestBase64InOneEnvelopeExceedsIPCFrame is the premise the whole chunked
// design rests on, asserted rather than assumed: the obvious encoding — the
// whole file base64'd into one JSON payload — does not fit in an IPC frame for
// any document-sized file, so no amount of tuning makes the simple version work.
func TestBase64InOneEnvelopeExceedsIPCFrame(t *testing.T) {
	for _, size := range []int{1 << 20, 3 << 20, 20 << 20} {
		payload, err := json.Marshal(map[string]string{
			"file_name":      "contract.docx",
			"content_base64": base64.StdEncoding.EncodeToString(randN(t, size)),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if len(payload) <= ipc.MaxFrameSize {
			t.Errorf("a %d-byte file base64s to a %d-byte envelope, which FITS in the %d-byte frame — the chunking premise needs revisiting",
				size, len(payload), ipc.MaxFrameSize)
		}
	}
	// And the converse: one chunk must comfortably fit, or chunking is useless.
	chunk, _ := json.Marshal(map[string]any{
		"blob_id": strings.Repeat("a", 32), "seq": 0,
		"data_base64": base64.StdEncoding.EncodeToString(randN(t, MaxChunkBytes)),
	})
	if len(chunk) > ipc.MaxFrameSize {
		t.Fatalf("one MaxChunkBytes chunk encodes to %d bytes, over the %d-byte frame", len(chunk), ipc.MaxFrameSize)
	}
}

// TestChunkedReassemblyIsExact: a file far larger than one frame comes back
// byte-identical after being pushed through in chunks.
func TestChunkedReassemblyIsExact(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	content := randN(t, 3<<20)
	id := stage(t, s, "mutual-nda.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", content)

	f, meta, err := s.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("reassembled %d bytes, want %d, and they differ", len(got), len(content))
	}
	if meta.FileName != "mutual-nda.docx" {
		t.Errorf("FileName = %q", meta.FileName)
	}
}

func TestChunkTooBigRefused(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, MaxChunkBytes+1)
	m, err := s.Begin("x.bin", "", int64(len(content)), sha(content))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := s.Append(m.ID, 0, content); !errors.Is(err, ErrChunkTooBig) {
		t.Fatalf("Append oversize chunk: err = %v, want ErrChunkTooBig", err)
	}
}

// TestOutOfOrderChunkRefused: tolerating a gap or a replay would reassemble
// something the caller never sent.
func TestOutOfOrderChunkRefused(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 4096)
	m, _ := s.Begin("x.bin", "", int64(len(content)), sha(content))
	if _, err := s.Append(m.ID, 0, content[:2048]); err != nil {
		t.Fatalf("Append 0: %v", err)
	}
	if _, err := s.Append(m.ID, 2, content[2048:]); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("skipped seq: err = %v, want ErrOutOfOrder", err)
	}
	if _, err := s.Append(m.ID, 0, content[2048:]); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("replayed seq: err = %v, want ErrOutOfOrder", err)
	}
}

// TestChecksumMismatchRefused: the declared sha is the integrity contract over
// the whole reassembly, so a body that does not match it must never be sendable.
func TestChecksumMismatchRefused(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 4096)
	tampered := append([]byte(nil), content...)
	tampered[0] ^= 0xff

	m, _ := s.Begin("x.bin", "", int64(len(content)), sha(content))
	if _, err := s.Append(m.ID, 0, tampered); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.Finalize(m.ID); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Finalize: err = %v, want ErrChecksum", err)
	}
	if _, _, err := s.Open(m.ID); err == nil {
		t.Fatal("a blob that failed its checksum is still openable")
	}
}

func TestOversizeAndIncompleteRefused(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 4096)
	m, _ := s.Begin("x.bin", "", int64(len(content)), sha(content))
	if _, err := s.Append(m.ID, 0, randN(t, 8192)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-declared write: err = %v, want ErrTooLarge", err)
	}
	if _, err := s.Finalize(m.ID); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("short blob: err = %v, want ErrIncomplete", err)
	}
}

// TestBlobIDIsNotCallerControlled: ids are minted, and anything that could
// contain a path separator is refused outright.
func TestBlobIDIsNotCallerControlled(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	for _, bad := range []string{
		"../../etc/passwd", "..", "/abs", "short", "",
		strings.Repeat("g", 32), // right length, not hex
		strings.Repeat("a", 33),
	} {
		if _, err := s.Append(bad, 0, []byte("x")); err == nil {
			t.Errorf("Append(%q) was accepted", bad)
		}
		if _, err := s.Finalize(bad); err == nil {
			t.Errorf("Finalize(%q) was accepted", bad)
		}
	}
	// Two Begins never collide on an id.
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		m, err := s.Begin("x", "", 8, sha([]byte("12345678")))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if seen[m.ID] {
			t.Fatalf("minted a duplicate blob id: %s", m.ID)
		}
		seen[m.ID] = true
	}
}

// TestFileNameCannotEscape: the caller's file name is metadata, never a path.
func TestFileNameCannotEscape(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	m, err := s.Begin("../../../etc/passwd", "", 4, sha([]byte("abcd")))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if strings.ContainsAny(m.FileName, `/\`) {
		t.Fatalf("FileName = %q, still carries path separators", m.FileName)
	}
	if m.FileName != "passwd" {
		t.Errorf("FileName = %q, want the base name only", m.FileName)
	}
}

func TestBadDeclaredSizeAndChecksumRefused(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if _, err := s.Begin("x", "", 0, sha([]byte("x"))); !errors.Is(err, ErrBadDeclared) {
		t.Errorf("zero size: %v", err)
	}
	if _, err := s.Begin("x", "", DefaultMaxBlobBytes+1, sha([]byte("x"))); !errors.Is(err, ErrBadDeclared) {
		t.Errorf("over cap: %v", err)
	}
	if _, err := s.Begin("x", "", 4, "nothex"); !errors.Is(err, ErrBadChecksum) {
		t.Errorf("bad sha: %v", err)
	}
	if _, err := s.Begin("  ", "", 4, sha([]byte("abcd"))); !errors.Is(err, ErrNameRequired) {
		t.Errorf("blank name: %v", err)
	}
}

// TestBuildFormRoundTrip: what the adapter assembles is what a FastAPI-shaped
// parser reads back, including the real content type of the file part.
func TestBuildFormRoundTrip(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 2<<20)
	docxCT := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	id := stage(t, s, "nda.docx", docxCT, content)

	f, meta, err := s.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	body, ct, err := BuildForm("file", meta.FileName, meta.ContentType, f, map[string]string{
		"deal_id":           "deal_123",
		"context_for_legal": "Standard mutual NDA.",
	})
	if err != nil {
		t.Fatalf("BuildForm: %v", err)
	}

	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q (mt %q, boundary %q)", ct, mt, params["boundary"])
	}

	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]string{}
	var fileSHA, fileName, filePartCT string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		b, _ := io.ReadAll(p)
		if p.FileName() != "" {
			fileName, fileSHA, filePartCT = p.FileName(), sha(b), p.Header.Get("Content-Type")
		} else {
			fields[p.FormName()] = string(b)
		}
		p.Close()
	}
	if fileSHA != sha(content) {
		t.Error("file bytes changed passing through BuildForm")
	}
	if fileName != "nda.docx" {
		t.Errorf("file name = %q", fileName)
	}
	if filePartCT != docxCT {
		t.Errorf("file part Content-Type = %q, want %q (octet-stream loses what the partner accepts on)", filePartCT, docxCT)
	}
	if fields["deal_id"] != "deal_123" {
		t.Errorf("deal_id = %q", fields["deal_id"])
	}
}

// TestBuildFormEscapesFileName: a quote in the file name must not break out of
// the Content-Disposition header and forge a part.
func TestBuildFormEscapesFileName(t *testing.T) {
	evil := `a".docx"; name="deal_id`
	body, ct, err := BuildForm("file", evil, "text/plain", strings.NewReader("x"), map[string]string{"deal_id": "real"})
	if err != nil {
		t.Fatalf("BuildForm: %v", err)
	}
	_, params, _ := mime.ParseMediaType(ct)
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	names := map[string]int{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		names[p.FormName()]++
		p.Close()
	}
	if names["deal_id"] != 1 {
		t.Fatalf("deal_id appears %d times — the file name forged a part", names["deal_id"])
	}
	if names["file"] != 1 {
		t.Fatalf("file part appears %d times", names["file"])
	}
}

func TestAbortAndGC(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	content := randN(t, 1024)
	m, _ := s.Begin("x.bin", "", int64(len(content)), sha(content))
	if _, err := s.Append(m.ID, 0, content); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Abort(m.ID); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := s.Append(m.ID, 1, content); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Abort: err = %v, want ErrNotFound", err)
	}

	s2, _ := NewStore(t.TempDir())
	s2.SetLimits(0, 1) // 1ns TTL: everything is immediately stale
	m2, _ := s2.Begin("y.bin", "", 16, sha(randN(t, 16)))
	if n := s2.GC(); n != 1 {
		t.Fatalf("GC reclaimed %d, want 1", n)
	}
	if _, err := s2.Append(m2.ID, 0, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after GC: err = %v, want ErrNotFound", err)
	}
}
