package broker

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression tests for the boundary conditions of multipart forwarding. Each
// one pins a bug that the happy-path tests in zz_multipart_test.go did not
// catch, which is exactly why they are worth keeping separate.

// glBrokerWithCap builds the General Legal broker with an explicit body cap so
// the 413 path is testable without pushing megabytes through every run.
func glBrokerWithCap(t *testing.T, got *[]glUpload, cap int64) (*Broker, func()) {
	t.Helper()
	b, done := glBroker(t, got)
	b.Registry().Get("io.pilot.generallegal").MaxBodyBytes = cap
	return b, done
}

// TestOversizeBody_413NotSilentTruncation: the broker used to read exactly
// MaxBody bytes, which truncated a larger body instead of refusing it. The
// caller then saw a signature failure or an opaque tenancy 404 — never "your
// file was too big". A truncated body must never be processed at all.
func TestOversizeBody_413NotSilentTruncation(t *testing.T) {
	var got []glUpload
	b, done := glBrokerWithCap(t, &got, 64<<10)
	defer done()
	_, alice := newKey(t)

	body, ct := buildUpload(t, "big.docx", randBytes(t, 256<<10), map[string]string{"deal_id": "d1"})
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body: status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(got) != 0 {
		t.Fatal("an oversize body reached the partner")
	}
}

// TestBodyExactlyAtCapIsAccepted is the other half: the cap is inclusive, so a
// body sitting exactly on it must still go through. Reading maxBody+1 to detect
// overflow is easy to get wrong by one byte in this direction.
func TestBodyExactlyAtCapIsAccepted(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)
	dealID := openDeal(t, b, alice)

	body, ct := buildUpload(t, "x.docx", randBytes(t, 4096), map[string]string{"deal_id": dealID})
	b.Registry().Get("io.pilot.generallegal").MaxBodyBytes = int64(len(body))

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != 200 {
		t.Fatalf("body exactly at the cap: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestPerAppCapOverridesBrokerDefault: the broker default is tuned for JSON and
// is smaller than an upload partner's own limit (8 MiB vs General Legal's 20),
// so without a per-app override the platform silently cannot carry uploads the
// partner would have accepted. Asserted by lowering the default rather than by
// pushing megabytes through, which tests the same branch far more cheaply.
func TestPerAppCapOverridesBrokerDefault(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)
	dealID := openDeal(t, b, alice)

	body, ct := buildUpload(t, "big.pdf", randBytes(t, 96<<10), map[string]string{"deal_id": dealID})

	// Below the broker-wide cap: refused.
	b.MaxBody = 32 << 10
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("under the broker default: status = %d, want 413", rec.Code)
	}

	// Same body, same broker, per-app cap raised: accepted.
	b.Registry().Get("io.pilot.generallegal").MaxBodyBytes = 1 << 20
	rec = httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != 200 {
		t.Fatalf("with the per-app cap raised: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0].FileBytes != 96<<10 {
		t.Fatalf("partner got %d uploads; want one of %d bytes", len(got), 96<<10)
	}
}

// buildParts assembles a body with n scalar fields plus one file, so the part
// budget can be exercised from both sides.
func buildParts(t *testing.T, n int, dealID string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if dealID != "" {
		_ = w.WriteField("deal_id", dealID)
		n--
	}
	fw, _ := w.CreateFormFile("file", "x.docx")
	_, _ = fw.Write([]byte("hello"))
	n--
	for i := 0; i < n; i++ {
		_ = w.WriteField(fmt.Sprintf("pad_%d", i), "v")
	}
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

// TestPartBudget_ExactlyAtLimitAccepted: the guard checked the counter before
// reading, so a body holding exactly maxMultipartParts parts was refused even
// though it was within budget.
func TestPartBudget_ExactlyAtLimitAccepted(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)
	dealID := openDeal(t, b, alice)

	body, ct := buildParts(t, maxMultipartParts, dealID)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != 200 {
		t.Fatalf("body with exactly %d parts: status = %d, want 200 (%s)", maxMultipartParts, rec.Code, rec.Body.String())
	}
}

func TestPartBudget_OverLimitRefused(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)
	dealID := openDeal(t, b, alice)

	body, ct := buildParts(t, maxMultipartParts+1, dealID)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("body with %d parts: status = %d, want 404", maxMultipartParts+1, rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("an over-budget body reached the partner")
	}
}

// TestDuplicateNonRefFieldAllowed: repeating a field name is legal multipart —
// a multi-file upload posts the same name several times. The duplicate ban
// exists to stop a parser differential on a field the broker made a decision
// about, so applying it to fields nobody checks only breaks working apps.
func TestDuplicateNonRefFieldAllowed(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)
	dealID := openDeal(t, b, alice)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("deal_id", dealID)
	_ = w.WriteField("tag", "one") // repeated, and not a ref
	_ = w.WriteField("tag", "two")
	fw, _ := w.CreateFormFile("file", "a.docx")
	_, _ = fw.Write([]byte("a"))
	fw2, _ := w.CreateFormFile("file", "b.docx") // repeated file part
	_, _ = fw2.Write([]byte("b"))
	_ = w.Close()

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", w.FormDataContentType(), buf.Bytes()))
	if rec.Code != 200 {
		t.Fatalf("duplicate non-ref fields: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestEmptyRefFieldSkipped: deal_id is optional on this endpoint (omitting it
// opens a fresh matter), so a present-but-empty field must not be looked up as
// a resource id and refused. Mirrors the JSON path, where the empty string is
// not an id.
func TestEmptyRefFieldSkipped(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)

	body, ct := buildUpload(t, "new.docx", []byte("x"), map[string]string{"deal_id": ""})
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != 200 {
		t.Fatalf("empty deal_id: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestOversizeRefFieldRefused: a ref value far larger than any id is a probe,
// not a document reference.
func TestOversizeRefFieldRefused(t *testing.T) {
	var got []glUpload
	b, done := glBroker(t, &got)
	defer done()
	_, alice := newKey(t)

	body, ct := buildUpload(t, "x.docx", []byte("x"), map[string]string{
		"deal_id": string(bytes.Repeat([]byte("a"), maxRefFieldBytes+1)),
	})
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedCT(t, alice, "POST", "/io.pilot.generallegal/api/v1/documents", ct, body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("oversize ref field: status = %d, want 404", rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("oversize-ref body reached the partner")
	}
}

// TestMultipartWithoutBodyRefsFailsBoot: an upload names its resource in a form
// field, so an app that forwards multipart with only param_types declared would
// ownership-check nothing on exactly the route that needs it most. The registry
// fails the boot rather than serve a spec that does not isolate — the same
// stance the rest of validateTenancy takes.
func TestMultipartWithoutBodyRefsFailsBoot(t *testing.T) {
	reg := `[{
      "id":"io.pilot.leaky","upstream":"http://x","key_env":"K",
      "auth_header":"Authorization",
      "allow":["POST /api/v1/deals","POST /api/v1/documents"],
      "forward_content_types":["multipart/form-data"],
      "tenancy":{
        "param_types":{"deal_id":"deal"},
        "create":[{"method":"POST","path":"/api/v1/deals","type":"deal","id_field":"deal_id"}]
      }}]`
	_, err := ParseRegistry([]byte(reg), func(string) string { return "k" })
	if err == nil {
		t.Fatal("a multipart app with no body_refs loaded successfully; it would forward unchecked uploads")
	}
	if !strings.Contains(err.Error(), "body_refs") {
		t.Fatalf("error should name the missing field, got: %v", err)
	}

	// The same spec WITH body_refs is fine.
	ok := strings.Replace(reg, `"param_types":{"deal_id":"deal"},`,
		`"param_types":{"deal_id":"deal"},"body_refs":{"deal_id":"deal"},`, 1)
	if _, err := ParseRegistry([]byte(ok), func(string) string { return "k" }); err != nil {
		t.Fatalf("valid multipart spec rejected: %v", err)
	}

	// And an app that does NOT forward multipart is unaffected by the new rule.
	noMP := strings.Replace(reg, `"forward_content_types":["multipart/form-data"],`, "", 1)
	if _, err := ParseRegistry([]byte(noMP), func(string) string { return "k" }); err != nil {
		t.Fatalf("non-multipart app newly rejected: %v", err)
	}
}
