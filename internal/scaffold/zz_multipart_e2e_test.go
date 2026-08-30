//go:build !windows

package scaffold

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	mrand "math/rand"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

// multipartSpec mirrors the General Legal document endpoint: a multipart POST
// whose file is staged out-of-band and whose deal_id rides along as a form
// field, plus a path-param variant to prove placeholders still resolve.
const multipartSpec = `
id: io.pilot.uploadx
app_version: 0.1.0
description: "App exercising multipart uploads."
namespace: uploadx
backend:
  base_url: https://placeholder.invalid
methods:
  - name: uploadx.document_upload
    summary: "Upload a document for review."
    duration: slow
    http:
      verb: POST
      path: /api/v1/documents
      multipart: { file_field: file, max_bytes: 8388608 }
    params:
      blob_id: "staged blob"
      deal_id: "matter to attach to"
      context_for_legal: "note for counsel"
  - name: uploadx.version_replace
    summary: "Upload into a path-scoped resource."
    duration: slow
    http:
      verb: POST
      path: "/api/v1/contracts/{contract_id}/versions"
      multipart: { file_field: document }
    params:
      blob_id: "staged blob"
      contract_id: "path param"
`

type gotUpload struct {
	path        string
	contentType string
	fileField   string
	fileName    string
	filePartCT  string
	sha         string
	size        int
	fields      map[string]string
}

// TestGeneratedMultipartUploadE2E is the end-to-end proof of the whole design:
// it scaffolds a real adapter, builds it, runs it as the daemon would, stages a
// file through the IPC chunk methods in pieces that each fit inside
// ipc.MaxFrameSize, then calls the upload method and asserts the partner
// received one well-formed multipart body with the bytes intact.
//
// It is deliberately driven over the real IPC socket rather than by calling the
// handlers directly: the frame limit is the constraint the whole design exists
// for, so a test that bypasses the transport would prove nothing.
func TestGeneratedMultipartUploadE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	var mu = make(chan *gotUpload, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			http.Error(w, `{"detail":"expected multipart/form-data, got `+r.Header.Get("Content-Type")+`"}`, 422)
			return
		}
		g := &gotUpload{path: r.URL.Path, contentType: r.Header.Get("Content-Type"), fields: map[string]string{}}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, `{"detail":"bad multipart"}`, 400)
				return
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				sum := sha256.Sum256(b)
				g.fileField, g.fileName = p.FormName(), p.FileName()
				g.sha, g.size = hex.EncodeToString(sum[:]), len(b)
				g.filePartCT = p.Header.Get("Content-Type")
			} else {
				g.fields[p.FormName()] = string(b)
			}
			p.Close()
		}
		mu <- g
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deal_id":"d1","contract_id":"c1","version_id":"v1"}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	cfg := parseSpec(t, multipartSpec)
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("spec invalid: %v", errs)
	}
	// The staging methods must be generated from the multipart route alone.
	for _, want := range []string{"uploadx.upload_begin", "uploadx.upload_chunk", "uploadx.upload_abort"} {
		found := false
		for i := range cfg.Methods {
			if cfg.Methods[i].Name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was not injected; a multipart route with no way to stage is unusable", want)
		}
	}

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

	sockDir, err := os.MkdirTemp("", "upsk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "a.sock")
	adapter := exec.Command(bin, "--socket", sock, "--manifest", filepath.Join(proj, "manifest.json"))
	adapter.Stderr = os.Stderr
	adapter.Env = append(os.Environ(), "UPLOADX_BACKEND_URL="+srv.URL)
	if err := adapter.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() { _ = adapter.Process.Kill(); _, _ = adapter.Process.Wait() }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	call := func(t *testing.T, method, args string) (json.RawMessage, error) {
		t.Helper()
		conn, err := net.DialTimeout("unix", sock, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		var out json.RawMessage
		err = ipc.Call(conn, method, json.RawMessage(args), &out)
		return out, err
	}
	mustCall := func(t *testing.T, method, args string) json.RawMessage {
		t.Helper()
		out, err := call(t, method, args)
		if err != nil {
			t.Fatalf("call %s: %v", method, err)
		}
		return out
	}

	// A 3 MiB document: three times the 1 MiB frame, so it provably cannot have
	// arrived in one envelope.
	content := make([]byte, 3<<20)
	_, _ = mrand.New(mrand.NewSource(7)).Read(content)
	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])

	stageFile := func(t *testing.T, name, ctype string, body []byte) string {
		t.Helper()
		s := sha256.Sum256(body)
		beginArgs, _ := json.Marshal(map[string]any{
			"file_name": name, "content_type": ctype,
			"total_bytes": len(body), "sha256": hex.EncodeToString(s[:]),
		})
		var begin struct {
			BlobID   string `json:"blob_id"`
			MaxChunk int    `json:"max_chunk_bytes"`
		}
		if err := json.Unmarshal(mustCall(t, "uploadx.upload_begin", string(beginArgs)), &begin); err != nil {
			t.Fatalf("decode upload_begin: %v", err)
		}
		if begin.BlobID == "" || begin.MaxChunk <= 0 {
			t.Fatalf("upload_begin returned %+v", begin)
		}
		chunks := 0
		for seq, off := 0, 0; off < len(body); seq, off = seq+1, off+begin.MaxChunk {
			end := off + begin.MaxChunk
			if end > len(body) {
				end = len(body)
			}
			args, _ := json.Marshal(map[string]any{
				"blob_id": begin.BlobID, "seq": seq,
				"data_base64": base64.StdEncoding.EncodeToString(body[off:end]),
			})
			// Every staged envelope must fit the frame, or the design does not work.
			if len(args) > ipc.MaxFrameSize {
				t.Fatalf("chunk envelope is %d bytes, over the %d-byte frame", len(args), ipc.MaxFrameSize)
			}
			var got struct {
				Complete bool  `json:"complete"`
				Received int64 `json:"received"`
			}
			if err := json.Unmarshal(mustCall(t, "uploadx.upload_chunk", string(args)), &got); err != nil {
				t.Fatalf("decode upload_chunk: %v", err)
			}
			chunks++
			wantComplete := end == len(body)
			if got.Complete != wantComplete {
				t.Fatalf("chunk %d: complete = %v, want %v (received %d of %d)", seq, got.Complete, wantComplete, got.Received, len(body))
			}
		}
		if len(body) > ipc.MaxFrameSize && chunks < 2 {
			t.Fatalf("a %d-byte file staged in %d chunk(s); it cannot have fit the frame", len(body), chunks)
		}
		return begin.BlobID
	}

	// --- the headline path -------------------------------------------------
	blobID := stageFile(t, "mutual-nda.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", content)

	args, _ := json.Marshal(map[string]any{
		"blob_id":           blobID,
		"deal_id":           "deal_abc",
		"context_for_legal": "Standard mutual NDA, we are the disclosing party.",
	})
	mustCall(t, "uploadx.document_upload", string(args))

	select {
	case g := <-mu:
		if g.path != "/api/v1/documents" {
			t.Errorf("path = %q", g.path)
		}
		if g.sha != wantSHA {
			t.Errorf("document corrupted end to end:\n got %s (%d bytes)\nwant %s (%d bytes)", g.sha, g.size, wantSHA, len(content))
		}
		if g.fileField != "file" {
			t.Errorf("file field = %q, want the configured %q", g.fileField, "file")
		}
		if g.fileName != "mutual-nda.docx" {
			t.Errorf("file name = %q", g.fileName)
		}
		if !strings.Contains(g.filePartCT, "wordprocessingml") {
			t.Errorf("file part Content-Type = %q, want the declared docx type", g.filePartCT)
		}
		if g.fields["deal_id"] != "deal_abc" {
			t.Errorf("deal_id = %q", g.fields["deal_id"])
		}
		if g.fields["context_for_legal"] == "" {
			t.Error("context_for_legal did not reach the partner")
		}
		if _, leaked := g.fields["blob_id"]; leaked {
			t.Error("blob_id was forwarded as a form field; it is adapter-internal")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("partner never received the upload")
	}

	// --- a path param must still resolve, and not become a form field ------
	small := []byte("second document, much smaller")
	blob2 := stageFile(t, "v2.txt", "text/plain", small)
	args2, _ := json.Marshal(map[string]any{"blob_id": blob2, "contract_id": "c-42"})
	mustCall(t, "uploadx.version_replace", string(args2))

	select {
	case g := <-mu:
		if g.path != "/api/v1/contracts/c-42/versions" {
			t.Errorf("path param not substituted: %q", g.path)
		}
		if _, leaked := g.fields["contract_id"]; leaked {
			t.Error("contract_id went into the form as well as the path")
		}
		if g.fileField != "document" {
			t.Errorf("file field = %q, want the per-route %q", g.fileField, "document")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("partner never received the second upload")
	}

	// --- failure paths, over the real transport ----------------------------

	// A blob is single-use: the adapter drops the local copy once the partner
	// has the bytes, so replaying the same id must fail rather than re-send.
	if _, err := call(t, "uploadx.document_upload", string(args)); err == nil {
		t.Error("re-using a consumed blob_id succeeded; the staged bytes should be gone")
	}

	// A checksum that does not match the staged bytes must be caught locally.
	badArgs, _ := json.Marshal(map[string]any{
		"file_name": "x.txt", "content_type": "text/plain",
		"total_bytes": 4, "sha256": strings.Repeat("a", 64),
	})
	var bad struct {
		BlobID string `json:"blob_id"`
	}
	_ = json.Unmarshal(mustCall(t, "uploadx.upload_begin", string(badArgs)), &bad)
	chunkArgs, _ := json.Marshal(map[string]any{
		"blob_id": bad.BlobID, "seq": 0,
		"data_base64": base64.StdEncoding.EncodeToString([]byte("abcd")),
	})
	if _, err := call(t, "uploadx.upload_chunk", string(chunkArgs)); err == nil {
		t.Error("a chunk completing a blob whose sha256 does not match was accepted")
	}

	// Uploading without staging anything must say so, not send an empty file.
	if _, err := call(t, "uploadx.document_upload", `{"deal_id":"d"}`); err == nil {
		t.Error("upload with no blob_id succeeded")
	}

	// abort makes the blob unusable.
	blob3 := stageFile(t, "gone.txt", "text/plain", []byte("temporary"))
	mustCall(t, "uploadx.upload_abort", `{"blob_id":"`+blob3+`"}`)
	a3, _ := json.Marshal(map[string]any{"blob_id": blob3, "deal_id": "d"})
	if _, err := call(t, "uploadx.document_upload", string(a3)); err == nil {
		t.Error("uploading an aborted blob succeeded")
	}

	// The generated help must advertise the staging steps, or an agent that
	// installs this app has no way to discover how to upload at all.
	help := mustCall(t, "uploadx.help", `{}`)
	for _, want := range []string{"upload_begin", "upload_chunk", "document_upload"} {
		if !strings.Contains(string(help), want) {
			t.Errorf("uploadx.help does not mention %q", want)
		}
	}
}
