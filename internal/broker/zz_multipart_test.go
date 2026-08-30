package broker

import (
	"bytes"
	"crypto/ed25519"
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
	"net/textproto"
	"testing"
	"time"
)

// Multipart uploads through a shared master key.
//
// The General Legal API's document upload is multipart/form-data, and the deal
// it uploads into arrives as a FORM FIELD, not a JSON key. That breaks two
// broker assumptions at once: the forward hardcoded application/json, and the
// tenancy body-ref check could only read JSON and so refused any body it could
// not parse. Refusing was the safe failure, but it made uploads impossible.
//
// These tests pin down both halves: the body must survive the hop intact, and
// the ownership check must still hold on a field the broker now has to parse
// out of a multipart envelope.

const glRegistryJSON = `[{
  "id": "io.pilot.generallegal",
  "upstream": "%s",
  "key_env": "GL_KEY",
  "auth_header": "Authorization",
  "auth_scheme": "Bearer",
  "quota": 0,
  "allow": [
    "POST /api/v1/deals", "GET /api/v1/deals",
    "GET /api/v1/deals/{deal_id}",
    "POST /api/v1/documents"
  ],
  "forward_content_types": ["multipart/form-data"],
  "tenancy": {
    "param_types": {"deal_id": "deal"},
    "body_refs":   {"deal_id": "deal"},
    "create": [
      {"method": "POST", "path": "/api/v1/deals", "type": "deal", "id_field": "deal_id"}
    ],
    "list": [
      {"method": "GET", "path": "/api/v1/deals", "array": "items",
       "owner_by": [{"field": "id", "type": "deal"}], "count_fields": ["total"]}
    ]
  }
}]`

// glUpload is what the partner actually received, so a test can assert the
// bytes crossed every hop unchanged.
type glUpload struct {
	ContentType string
	FileName    string
	FileSHA256  string
	FileBytes   int
	Fields      map[string]string
	Auth        string
}

// glUpstream fakes api.general.legal's document endpoint: it parses the
// multipart body the way FastAPI would and records what arrived.
func glUpstream(t *testing.T, got *[]glUpload) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/deals":
			fmt.Fprintf(w, `{"deal_id":"deal_%d","status":"open"}`, time.Now().UnixNano())

		case r.Method == "POST" && r.URL.Path == "/api/v1/documents":
			mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "multipart/form-data" {
				http.Error(w, `{"detail":"expected multipart/form-data"}`, 422)
				return
			}
			u := glUpload{
				ContentType: r.Header.Get("Content-Type"),
				Fields:      map[string]string{},
				Auth:        r.Header.Get("Authorization"),
			}
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					http.Error(w, `{"detail":"malformed multipart"}`, 400)
					return
				}
				b, _ := io.ReadAll(p)
				if p.FileName() != "" {
					sum := sha256.Sum256(b)
					u.FileName = p.FileName()
					u.FileSHA256 = hex.EncodeToString(sum[:])
					u.FileBytes = len(b)
				} else {
					u.Fields[p.FormName()] = string(b)
				}
				p.Close()
			}
			if got != nil {
				*got = append(*got, u)
			}
			fmt.Fprint(w, `{"deal_id":"d1","contract_id":"c1","version_id":"v1"}`)

		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
}

func glBroker(t *testing.T, got *[]glUpload) (*Broker, func()) {
	t.Helper()
	up := glUpstream(t, got)
	reg, err := ParseRegistry([]byte(fmt.Sprintf(glRegistryJSON, up.URL)),
		func(string) string { return "glk_master" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	return b, up.Close
}

// signedCT is signedReq plus an explicit Content-Type. The signature covers
// method, path, timestamp and a hash of the body — not the header — so setting
// it after signing is correct and mirrors what the adapter does.
func signedCT(t *testing.T, priv ed25519.PrivateKey, method, path, ct string, body []byte) *http.Request {
	t.Helper()
	req := signedReq(t, priv, method, path, body, time.Now())
	req.Header.Set("Content-Type", ct)
	return req
}

// openDeal creates a deal as the given caller and returns its id, so the
// upload tests have a resource that is genuinely owned.
func openDeal(t *testing.T, b *Broker, priv ed25519.PrivateKey) string {
	t.Helper()
	rec := do(t, b, priv, "POST", "/io.pilot.generallegal/api/v1/deals", []byte(`{"initial_request":"review an NDA"}`))
	if rec.Code != 200 {
		t.Fatalf("open deal: status %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		DealID string `json:"deal_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.DealID == "" {
		t.Fatalf("open deal: bad response %s", rec.Body.String())
	}
	return out.DealID
}

// buildUpload assembles a document upload body the way the adapter does.
func buildUpload(t *testing.T, fileName string, content []byte, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	h.Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// randBytes builds test content from a seeded PRNG rather than crypto/rand.
// The bytes only need to be non-uniform enough that a truncation or a corrupted
// hop changes the sha256; paying for cryptographic randomness by the megabyte
// just makes the package slow.
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	r := mrand.New(mrand.NewSource(int64(n)))
	_, _ = r.Read(b)
	return b
}

// TestMultipart_UploadReachesPartnerIntact is the headline case: a 3 MiB
// document — comfortably past the 1 MiB IPC frame that forced the chunked
// staging design — arrives at the partner byte-identical, with its boundary
// intact and the master key injected.
func TestMultipart_UploadReachesPartnerIntact(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	_, alice := newKey(t)

	dealID := openDeal(t, b, alice)
	content := randBytes(t, 3<<20)
	want := sha256.Sum256(content)

	body, ct := buildUpload(t, "mutual-nda.docx", content, map[string]string{
		"deal_id":           dealID,
		"context_for_legal": "Standard mutual NDA, we are the disclosing party.",
		"last_edit_by":      "client",
	})

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("partner received %d uploads, want 1", len(got))
	}
	u := got[0]
	if u.FileSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("file corrupted in transit:\n got sha %s (%d bytes)\nwant sha %s (%d bytes)",
			u.FileSHA256, u.FileBytes, hex.EncodeToString(want[:]), len(content))
	}
	if u.FileName != "mutual-nda.docx" {
		t.Errorf("file name = %q, want mutual-nda.docx", u.FileName)
	}
	if u.Fields["deal_id"] != dealID {
		t.Errorf("deal_id = %q, want %q", u.Fields["deal_id"], dealID)
	}
	if u.Fields["context_for_legal"] == "" {
		t.Error("context_for_legal did not survive the hop")
	}
	if u.Auth != "Bearer glk_master" {
		t.Errorf("Authorization = %q, want the injected master key", u.Auth)
	}
	if mt, params, _ := mime.ParseMediaType(u.ContentType); mt != "multipart/form-data" || params["boundary"] == "" {
		t.Errorf("Content-Type = %q, want multipart/form-data with a boundary", u.ContentType)
	}
}

// TestMultipart_CannotUploadIntoAnotherTenantsDeal is the isolation case the
// JSON path already had and the multipart path must not lose: Mallory names
// Alice's deal in a form field. Before the multipart body-ref check existed,
// this body was simply unparseable — which denied Alice too.
func TestMultipart_CannotUploadIntoAnotherTenantsDeal(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	aliceDeal := openDeal(t, b, alice)

	body, ct := buildUpload(t, "exfil.docx", randBytes(t, 4096), map[string]string{
		"deal_id": aliceDeal,
	})
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, mallory, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("Mallory uploading into Alice's deal: status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(got) != 0 {
		t.Fatalf("refused upload still reached the partner (%d times)", len(got))
	}

	// Paired assertion: the check denies the impostor without denying the owner.
	okBody, okCT := buildUpload(t, "ok.docx", randBytes(t, 4096), map[string]string{"deal_id": aliceDeal})
	rec2 := httptest.NewRecorder()
	b.ServeHTTP(rec2, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", okCT, okBody))
	if rec2.Code != 200 {
		t.Fatalf("Alice's own upload: status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
}

// TestMultipart_DuplicateFieldRefused closes the multipart twin of the
// duplicate-JSON-key parser differential: Go hands us every part, other stacks
// keep the first or the last, so a body naming deal_id twice could be validated
// as one value and acted on as another.
func TestMultipart_DuplicateFieldRefused(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	aliceDeal := openDeal(t, b, alice)
	mallDeal := openDeal(t, b, mallory)

	// Mallory's own deal first (which we would validate), Alice's second.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("deal_id", mallDeal)
	_ = w.WriteField("deal_id", aliceDeal)
	fw, _ := w.CreateFormFile("file", "x.docx")
	_, _ = fw.Write(randBytes(t, 512))
	_ = w.Close()

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, mallory, "POST", "/io.pilot.generallegal/api/v1/documents", w.FormDataContentType(), buf.Bytes()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("duplicate deal_id: status = %d, want 404", rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("body with duplicate refs reached the partner")
	}
}

// TestMultipart_RefSmuggledAsFilePartRefused: file parts are not inspected, so
// a ref arriving with a filename would otherwise slip past the ownership check
// while the partner still reads it as a field.
func TestMultipart_RefSmuggledAsFilePartRefused(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	_, alice := newKey(t)
	_, mallory := newKey(t)
	aliceDeal := openDeal(t, b, alice)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("deal_id", "deal_id.txt") // a ref wearing a filename
	_, _ = fw.Write([]byte(aliceDeal))
	fw2, _ := w.CreateFormFile("file", "x.docx")
	_, _ = fw2.Write(randBytes(t, 512))
	_ = w.Close()

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, mallory, "POST", "/io.pilot.generallegal/api/v1/documents", w.FormDataContentType(), buf.Bytes()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("ref smuggled as a file part: status = %d, want 404", rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("smuggled-ref body reached the partner")
	}
}

// TestMultipart_MalformedBodyRefused keeps the fail-closed property: a body we
// cannot parse is a body whose refs we cannot check.
func TestMultipart_MalformedBodyRefused(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	_, alice := newKey(t)

	junk := []byte("--boundary\r\nnot actually a multipart body")
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents",
		"multipart/form-data; boundary=boundary", junk))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed multipart: status = %d, want 404", rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("unparseable body reached the partner")
	}
}

// TestMultipart_MissingBoundaryRefused: without a boundary the body is
// undecodable, so it must not be forwarded on the hope the partner copes.
func TestMultipart_MissingBoundaryRefused(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	_, alice := newKey(t)
	body, _ := buildUpload(t, "x.docx", randBytes(t, 256), map[string]string{"deal_id": "d"})

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents",
		"multipart/form-data", body))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing boundary: status = %d, want 404", rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("boundary-less body reached the partner")
	}
}

// TestForwardContentType_DefaultsToJSON is the regression guard on the opt-in:
// an app that does NOT list multipart must still see the historical behaviour,
// so enabling this for one partner cannot quietly change every other one.
func TestForwardContentType_DefaultsToJSON(t *testing.T) {
	reg, err := ParseRegistry([]byte(`[{
      "id":"io.pilot.plain","upstream":"http://x","key_env":"K",
      "auth_header":"Authorization","allow":["POST /p"]}]`),
		func(string) string { return "k" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	app := reg.Get("io.pilot.plain")
	for _, in := range []string{
		"multipart/form-data; boundary=abc",
		"application/x-www-form-urlencoded",
		"text/plain",
		"",
	} {
		if got := app.forwardContentType(in); got != "application/json" {
			t.Errorf("forwardContentType(%q) = %q, want application/json", in, got)
		}
	}
}

// TestForwardContentType_AllowListed: only the listed type is passed through,
// and it is passed through VERBATIM so the boundary parameter survives.
func TestForwardContentType_AllowListed(t *testing.T) {
	var got []glUpload
	b, closeUp := glBroker(t, &got)
	defer closeUp()
	app := b.Registry().Get("io.pilot.generallegal")

	in := "multipart/form-data; boundary=xyz123"
	if out := app.forwardContentType(in); out != in {
		t.Errorf("allow-listed type = %q, want it forwarded verbatim (%q)", out, in)
	}
	// Still an allow-list, not a passthrough: an unlisted type is forced back.
	if out := app.forwardContentType("application/x-www-form-urlencoded"); out != "application/json" {
		t.Errorf("unlisted type = %q, want application/json", out)
	}
	if out := app.forwardContentType("!! not a media type"); out != "application/json" {
		t.Errorf("unparseable type = %q, want application/json", out)
	}
}
