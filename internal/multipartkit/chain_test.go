package multipartkit_test

// The whole path, in one test: an agent stages a document in IPC-sized chunks,
// the adapter reassembles it and builds one multipart body, signs it, and the
// real broker verifies the caller, checks that the deal in the form field is
// theirs, injects the master key, and forwards it to the partner.
//
// Each half is covered on its own (blob_test.go, internal/broker/zz_multipart_test.go).
// This exists for the seams between them, which is where a design like this
// actually breaks: a boundary lost at a hop, a body hashed before it was
// finished, a field the tenancy layer reads differently from the partner.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
	"github.com/pilot-protocol/app-template/internal/multipartkit"
)

const chainRegistry = `[{
  "id": "io.pilot.generallegal",
  "upstream": "%s",
  "key_env": "GL_KEY",
  "auth_header": "Authorization",
  "auth_scheme": "Bearer",
  "quota": 0,
  "allow": ["POST /api/v1/deals", "POST /api/v1/documents"],
  "forward_content_types": ["multipart/form-data"],
  "tenancy": {
    "param_types": {"deal_id": "deal"},
    "body_refs":   {"deal_id": "deal"},
    "create": [{"method":"POST","path":"/api/v1/deals","type":"deal","id_field":"deal_id"}],
    "list":   []
  }
}]`

type received struct {
	sha    string
	bytes  int
	name   string
	fields map[string]string
	auth   string
}

func partnerServer(t *testing.T, got *received) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/deals" {
			fmt.Fprintf(w, `{"deal_id":"deal_%d"}`, time.Now().UnixNano())
			return
		}
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			http.Error(w, `{"detail":"not multipart"}`, 422)
			return
		}
		rec := received{fields: map[string]string{}, auth: r.Header.Get("Authorization")}
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
				rec.sha, rec.bytes, rec.name = hex.EncodeToString(s[:]), len(b), p.FileName()
			} else {
				rec.fields[p.FormName()] = string(b)
			}
			p.Close()
		}
		*got = rec
		fmt.Fprint(w, `{"deal_id":"d1","contract_id":"c1","version_id":"v1"}`)
	}))
}

// send signs a request the way the generated adapter does and runs it through
// the broker. The signature covers the body bytes, so it has to be taken after
// the multipart body is fully assembled.
func send(t *testing.T, b *broker.Broker, priv ed25519.PrivateKey, method, path, ct string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range broker.Sign(priv, method, path, body, time.Now()) {
		req.Header.Set(k, v)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	return rec
}

func TestFullChain_ChunkedStagingToPartner(t *testing.T) {
	var got received
	up := partnerServer(t, &got)
	defer up.Close()

	reg, err := broker.ParseRegistry([]byte(fmt.Sprintf(chainRegistry, up.URL)),
		func(string) string { return "glk_master" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	bk := broker.New(reg, broker.NewMemStore())
	bk.Verify = broker.VerifyConfig{Window: time.Hour}

	_, alice, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_, mallory, _ := ed25519.GenerateKey(rand.Reader)

	// 1. Alice opens a matter. The broker claims the deal for her.
	rec := send(t, bk, alice, "POST", "/io.pilot.generallegal/api/v1/deals", "application/json",
		[]byte(`{"initial_request":"Review this mutual NDA"}`))
	if rec.Code != 200 {
		t.Fatalf("open deal: %d (%s)", rec.Code, rec.Body.String())
	}
	var deal struct {
		DealID string `json:"deal_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deal); err != nil {
		t.Fatalf("decode deal: %v", err)
	}

	// 2. The agent stages a 5 MiB document in IPC-sized chunks. Five megabytes
	//    is five times the frame that made the naive encoding impossible.
	// Seeded, not cryptographic: the size is the point (five times the 1 MiB
	// frame), and sha256 catches any corruption regardless of where the bytes
	// came from.
	content := make([]byte, 5<<20)
	_, _ = mrand.New(mrand.NewSource(1)).Read(content)
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	store, err := multipartkit.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	docxCT := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	meta, err := store.Begin("mutual-nda.docx", docxCT, int64(len(content)), want)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	chunks := 0
	for seq, off := 0, 0; off < len(content); seq, off = seq+1, off+multipartkit.MaxChunkBytes {
		end := off + multipartkit.MaxChunkBytes
		if end > len(content) {
			end = len(content)
		}
		if _, err := store.Append(meta.ID, seq, content[off:end]); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		chunks++
	}
	if _, err := store.Finalize(meta.ID); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if chunks < 5 {
		t.Fatalf("staged in %d chunks; expected the file to span several frames", chunks)
	}

	// 3. The adapter builds one multipart body from the staged blob.
	f, m, err := store.Open(meta.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, ct, err := multipartkit.BuildForm("file", m.FileName, m.ContentType, f, map[string]string{
		"deal_id":           deal.DealID,
		"context_for_legal": "Standard mutual NDA, we are the disclosing party.",
		"last_edit_by":      "client",
	})
	f.Close()
	if err != nil {
		t.Fatalf("BuildForm: %v", err)
	}

	// 4. Signed, brokered, forwarded.
	rec = send(t, bk, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body)
	if rec.Code != 200 {
		t.Fatalf("upload: %d (%s)", rec.Code, rec.Body.String())
	}
	if got.sha != want {
		t.Errorf("document corrupted end to end:\n got %s (%d bytes)\nwant %s (%d bytes)",
			got.sha, got.bytes, want, len(content))
	}
	if got.name != "mutual-nda.docx" {
		t.Errorf("file name = %q", got.name)
	}
	if got.fields["deal_id"] != deal.DealID {
		t.Errorf("deal_id = %q, want %q", got.fields["deal_id"], deal.DealID)
	}
	if got.auth != "Bearer glk_master" {
		t.Errorf("Authorization = %q — the master key was not injected", got.auth)
	}

	// 5. And the isolation the shared key makes necessary still holds on the far
	//    side of all that machinery: Mallory reuses Alice's staged bytes and her
	//    deal id, signs as herself, and is refused before the partner is touched.
	before := got
	rec = send(t, bk, mallory, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Mallory uploading into Alice's deal: %d, want 404", rec.Code)
	}
	if got.sha != before.sha || got.fields["deal_id"] != before.fields["deal_id"] {
		t.Fatal("the refused upload still reached the partner")
	}
}

// TestFullChain_TamperedBodyFailsSignature: the signature is taken over the
// assembled multipart bytes, so flipping one byte of the file after signing
// must be rejected as an identity failure rather than silently forwarded.
func TestFullChain_TamperedBodyFailsSignature(t *testing.T) {
	var got received
	up := partnerServer(t, &got)
	defer up.Close()

	reg, _ := broker.ParseRegistry([]byte(fmt.Sprintf(chainRegistry, up.URL)),
		func(string) string { return "glk_master" })
	bk := broker.New(reg, broker.NewMemStore())
	bk.Verify = broker.VerifyConfig{Window: time.Hour}
	_, alice, _ := ed25519.GenerateKey(rand.Reader)

	body, ct, err := multipartkit.BuildForm("file", "x.docx", "text/plain",
		bytes.NewReader([]byte("original contents")), map[string]string{"deal_id": "d1"})
	if err != nil {
		t.Fatalf("BuildForm: %v", err)
	}

	path := "/io.pilot.generallegal/api/v1/documents"
	req := httptest.NewRequest("POST", path, nil)
	for k, v := range broker.Sign(alice, "POST", path, body, time.Now()) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", ct)

	tampered := bytes.Replace(body, []byte("original"), []byte("modified"), 1)
	if bytes.Equal(tampered, body) {
		t.Fatal("test setup: body was not actually tampered with")
	}
	req.Body = io.NopCloser(bytes.NewReader(tampered))

	rec := httptest.NewRecorder()
	bk.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered multipart body: %d, want 401", rec.Code)
	}
}
