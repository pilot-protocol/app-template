//go:build !windows

package scaffold

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/pilot-protocol/app-template/internal/broker"
)

// The production topology for a managed upload app, assembled for real:
//
//	generated adapter --signed--> broker (master key + tenancy) --> partner
//
// The adapter and the broker are each covered on their own. This is the seam:
// the adapter signs a multipart body it assembled, and the broker has to verify
// that signature, parse the SAME body to ownership-check a form field, forward
// it with the boundary intact, and inject a key the adapter never sees. Those
// two halves were written against each other's contract, and this is the only
// test where that contract is actually exercised rather than assumed.
const managedUploadSpec = `
id: io.pilot.uploadz
app_version: 0.1.0
description: "Managed multipart upload app."
namespace: uploadz
backend:
  base_url: https://placeholder.invalid
  auth: managed
methods:
  - name: uploadz.deal_open
    summary: "Open a matter."
    http: { verb: POST, path: /api/v1/deals }
    params: { initial_request: "what you need" }
  - name: uploadz.document_upload
    summary: "Upload a document into a matter."
    duration: slow
    http:
      verb: POST
      path: /api/v1/documents
      multipart: { file_field: file }
    params:
      blob_id: "staged blob"
      deal_id: "matter to attach to"
`

const managedUploadRegistry = `[{
  "id": "io.pilot.uploadz",
  "upstream": "%s",
  "key_env": "UPLOADZ_KEY",
  "auth_header": "Authorization",
  "auth_scheme": "Bearer",
  "quota": 0,
  "allow": ["POST /api/v1/deals", "POST /api/v1/documents"],
  "forward_content_types": ["multipart/form-data"],
  "max_body_bytes": 16777216,
  "tenancy": {
    "param_types": {"deal_id": "deal"},
    "body_refs":   {"deal_id": "deal"},
    "create": [{"method":"POST","path":"/api/v1/deals","type":"deal","id_field":"deal_id"}],
    "list": []
  }
}]`

func TestGeneratedManagedUploadThroughBrokerE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real adapter binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	type arrived struct {
		sha    string
		size   int
		name   string
		auth   string
		fields map[string]string
	}
	got := make(chan arrived, 4)

	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/deals" {
			fmt.Fprintf(w, `{"deal_id":"deal_%d"}`, time.Now().UnixNano())
			return
		}
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			http.Error(w, `{"detail":"not multipart: `+r.Header.Get("Content-Type")+`"}`, 422)
			return
		}
		a := arrived{fields: map[string]string{}, auth: r.Header.Get("Authorization")}
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
				s := sha256.Sum256(b)
				a.sha, a.size, a.name = hex.EncodeToString(s[:]), len(b), p.FileName()
			} else {
				a.fields[p.FormName()] = string(b)
			}
			p.Close()
		}
		got <- a
		fmt.Fprint(w, `{"deal_id":"d1","contract_id":"c1","version_id":"v1"}`)
	}))
	defer partner.Close()

	reg, err := broker.ParseRegistry([]byte(fmt.Sprintf(managedUploadRegistry, partner.URL)),
		func(string) string { return "glk_master" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	bk := broker.New(reg, broker.NewMemStore())
	bk.Verify = broker.VerifyConfig{Window: time.Hour}
	brokerSrv := httptest.NewServer(bk)
	defer brokerSrv.Close()

	root := t.TempDir()
	cfg := parseSpec(t, managedUploadSpec)
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("spec invalid: %v", errs)
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

	// The daemon provisions a per-app ed25519 identity; stand in for it.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	idFile := filepath.Join(root, "identity.key")
	if err := os.WriteFile(idFile, []byte(base64.StdEncoding.EncodeToString(priv.Seed())), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	sockDir, err := os.MkdirTemp("", "mbsk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "a.sock")

	adapter := exec.Command(bin,
		"--socket", sock,
		"--manifest", filepath.Join(proj, "manifest.json"),
		"--identity", idFile)
	adapter.Stderr = os.Stderr
	adapter.Env = append(os.Environ(), "UPLOADZ_BACKEND_URL="+brokerSrv.URL+"/io.pilot.uploadz")
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

	call := func(method, args string) (json.RawMessage, error) {
		conn, err := net.DialTimeout("unix", sock, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		var out json.RawMessage
		err = ipc.Call(conn, method, json.RawMessage(args), &out)
		return out, err
	}
	mustCall := func(method, args string) json.RawMessage {
		t.Helper()
		out, err := call(method, args)
		if err != nil {
			t.Fatalf("call %s: %v", method, err)
		}
		return out
	}

	// 1. Open a matter. The broker claims the deal for this adapter's identity.
	var deal struct {
		DealID string `json:"deal_id"`
	}
	if err := json.Unmarshal(mustCall("uploadz.deal_open", `{"initial_request":"review an NDA"}`), &deal); err != nil {
		t.Fatalf("decode deal: %v", err)
	}
	if deal.DealID == "" {
		t.Fatal("no deal_id returned")
	}

	// 2. Stage a 2 MiB document through the frame-limited transport.
	content := make([]byte, 2<<20)
	_, _ = mrand.New(mrand.NewSource(11)).Read(content)
	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])

	beginArgs, _ := json.Marshal(map[string]any{
		"file_name": "nda.docx", "content_type": "application/pdf",
		"total_bytes": len(content), "sha256": wantSHA,
	})
	var begin struct {
		BlobID   string `json:"blob_id"`
		MaxChunk int    `json:"max_chunk_bytes"`
	}
	if err := json.Unmarshal(mustCall("uploadz.upload_begin", string(beginArgs)), &begin); err != nil {
		t.Fatalf("decode begin: %v", err)
	}
	for seq, off := 0, 0; off < len(content); seq, off = seq+1, off+begin.MaxChunk {
		end := off + begin.MaxChunk
		if end > len(content) {
			end = len(content)
		}
		a, _ := json.Marshal(map[string]any{
			"blob_id": begin.BlobID, "seq": seq,
			"data_base64": base64.StdEncoding.EncodeToString(content[off:end]),
		})
		mustCall("uploadz.upload_chunk", string(a))
	}

	// 3. Upload it into the matter, through the broker.
	upArgs, _ := json.Marshal(map[string]any{"blob_id": begin.BlobID, "deal_id": deal.DealID})
	mustCall("uploadz.document_upload", string(upArgs))

	select {
	case a := <-got:
		if a.sha != wantSHA {
			t.Errorf("document corrupted adapter->broker->partner:\n got %s (%d bytes)\nwant %s (%d bytes)",
				a.sha, a.size, wantSHA, len(content))
		}
		if a.name != "nda.docx" {
			t.Errorf("file name = %q", a.name)
		}
		if a.fields["deal_id"] != deal.DealID {
			t.Errorf("deal_id = %q, want %q", a.fields["deal_id"], deal.DealID)
		}
		if a.auth != "Bearer glk_master" {
			t.Errorf("Authorization = %q — the broker did not inject the master key", a.auth)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("partner never received the upload")
	}

	// 4. The isolation the shared key makes necessary, asserted through the real
	//    adapter: a deal this identity does not own is refused by the broker
	//    even though the adapter happily assembled and signed the body.
	content2 := []byte("someone else's matter")
	s2 := sha256.Sum256(content2)
	b2, _ := json.Marshal(map[string]any{
		"file_name": "x.txt", "content_type": "text/plain",
		"total_bytes": len(content2), "sha256": hex.EncodeToString(s2[:]),
	})
	var begin2 struct {
		BlobID string `json:"blob_id"`
	}
	_ = json.Unmarshal(mustCall("uploadz.upload_begin", string(b2)), &begin2)
	c2, _ := json.Marshal(map[string]any{
		"blob_id": begin2.BlobID, "seq": 0,
		"data_base64": base64.StdEncoding.EncodeToString(content2),
	})
	mustCall("uploadz.upload_chunk", string(c2))

	foreign, _ := json.Marshal(map[string]any{"blob_id": begin2.BlobID, "deal_id": "deal_not_mine"})
	if _, err := call("uploadz.document_upload", string(foreign)); err == nil {
		t.Error("uploading into an unowned deal succeeded")
	} else if !strings.Contains(err.Error(), "404") && !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Errorf("want the broker's opaque 404 for an unowned resource, got: %v", err)
	}
	select {
	case a := <-got:
		t.Fatalf("the refused upload still reached the partner: %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}
