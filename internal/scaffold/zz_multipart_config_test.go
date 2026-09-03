package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// specWith builds a one-method multipart spec with the given http: block, so a
// validation rule can be exercised without repeating the whole document.
func specWith(httpBlock string) string {
	return `
id: io.pilot.mpx
app_version: 0.1.0
description: "multipart validation fixture"
namespace: mpx
backend:
  base_url: https://placeholder.invalid
methods:
  - name: mpx.up
    summary: "upload"
    http:` + httpBlock + `
    params:
      blob_id: "staged blob"
`
}

// validateSpec parses + resolves + validates, returning the joined errors.
func validateSpec(t *testing.T, doc string) string {
	t.Helper()
	c, err := Parse([]byte(doc))
	if err != nil {
		return err.Error()
	}
	c.Resolve()
	errs := c.Validate()
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "; ")
}

// TestMultipartValidationRules: each of these specs would generate an adapter
// that fails only when a real upload is attempted against the partner, so they
// have to fail at build time instead.
func TestMultipartValidationRules(t *testing.T) {
	cases := []struct {
		name  string
		http  string
		want  string
		valid bool
	}{
		{
			name:  "a body verb is fine",
			http:  " { verb: POST, path: /docs, multipart: {} }",
			valid: true,
		},
		{
			name: "GET has nowhere to put a form",
			http: " { verb: GET, path: /docs, multipart: {} }",
			want: "needs a body verb",
		},
		{
			name: "blob_param must not be the file field",
			http: " { verb: POST, path: /docs, multipart: { file_field: blob_id } }",
			want: "the blob id names the staged file",
		},
		{
			name: "blob_param must stay in the body",
			http: " { verb: POST, path: /docs, multipart: {}, param_in: { blob_id: query } }",
			want: "must stay in the body",
		},
		{
			name: "max_bytes must be positive",
			http: " { verb: POST, path: /docs, multipart: { max_bytes: -1 } }",
			want: "max_bytes must be positive",
		},
		{
			name: "a raw body param conflicts with the form",
			http: " { verb: POST, path: /docs, multipart: {}, param_in: { blob_id: body_raw } }",
			want: "cannot be combined with a raw body param",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateSpec(t, specWith(tc.http))
			if tc.valid {
				if got != "" {
					t.Fatalf("valid spec rejected: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want an error mentioning %q, got: %s", tc.want, got)
			}
		})
	}
}

// TestMultipartDefaultsResolved: an author writing `multipart: {}` gets the
// conventional field names rather than empty strings that would produce a form
// the partner cannot read.
func TestMultipartDefaultsResolved(t *testing.T) {
	c, err := Parse([]byte(specWith(" { verb: POST, path: /docs, multipart: {} }")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c.Resolve()
	mp := c.Methods[0].HTTP.Multipart
	if mp.FileField != DefaultFileField {
		t.Errorf("file_field = %q, want %q", mp.FileField, DefaultFileField)
	}
	if mp.BlobParam != DefaultBlobParam {
		t.Errorf("blob_param = %q, want %q", mp.BlobParam, DefaultBlobParam)
	}
	if mp.MaxBytes != DefaultMaxBlobBytes {
		t.Errorf("max_bytes = %d, want %d", mp.MaxBytes, DefaultMaxBlobBytes)
	}
	if !c.HasMultipart() {
		t.Error("HasMultipart() = false for a spec with a multipart route")
	}
	if c.MaxBlobBytes() != DefaultMaxBlobBytes {
		t.Errorf("MaxBlobBytes() = %d", c.MaxBlobBytes())
	}
}

// TestStagingMethodsInjectedOnceAndOnlyWhenNeeded: the three staging steps are
// what make an upload reachable at all, so they must appear for a multipart app
// — and must NOT appear for an app without one, where they would be three dead
// methods on the store page.
func TestStagingMethodsInjectedOnceAndOnlyWhenNeeded(t *testing.T) {
	c, _ := Parse([]byte(specWith(" { verb: POST, path: /docs, multipart: {} }")))
	c.Resolve()
	c.Resolve() // idempotent: re-resolving must not duplicate the injected methods
	counts := map[string]int{}
	for i := range c.Methods {
		counts[c.Methods[i].Name]++
	}
	for _, want := range []string{"mpx.upload_begin", "mpx.upload_chunk", "mpx.upload_abort"} {
		if counts[want] != 1 {
			t.Errorf("%s appears %d times, want exactly 1", want, counts[want])
		}
	}

	plain, _ := Parse([]byte(`
id: io.pilot.plainx
app_version: 0.1.0
description: "no uploads here"
namespace: plainx
backend:
  base_url: https://placeholder.invalid
methods:
  - name: plainx.ping
    summary: "ping"
    http: { verb: GET, path: /ping }
`))
	plain.Resolve()
	if plain.HasMultipart() {
		t.Fatal("HasMultipart() = true for a spec with no multipart route")
	}
	for i := range plain.Methods {
		if strings.Contains(plain.Methods[i].Name, "upload_") {
			t.Errorf("staging method %q injected into an app with no multipart route", plain.Methods[i].Name)
		}
	}
}

// TestMultipartGrantsAndFiles: the manifest must grant exactly $APP/blobs (not
// $APP, which native delivery uses and which is far wider than an upload needs),
// and the staging runtime must actually be emitted.
func TestMultipartGrantsAndFiles(t *testing.T) {
	c, _ := Parse([]byte(specWith(" { verb: POST, path: /docs, multipart: {} }")))
	c.Resolve()
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatalf("spec invalid: %v", errs)
	}
	dir := t.TempDir()
	if _, err := Generate(c, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}

	man, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{
		`{"cap": "fs.read", "target": "$APP/blobs"}`,
		`{"cap": "fs.write", "target": "$APP/blobs"}`,
	} {
		if !strings.Contains(string(man), want) {
			t.Errorf("manifest is missing grant %s", want)
		}
	}
	if strings.Contains(string(man), `"fs.write", "target": "$APP"}`) {
		t.Error("manifest grants fs.write on all of $APP; uploads only need $APP/blobs")
	}
	for _, want := range []string{"mpx.upload_begin", "mpx.upload_chunk", "mpx.upload_abort", "mpx.up"} {
		if !strings.Contains(string(man), `"`+want+`"`) {
			t.Errorf("manifest exposes list is missing %q", want)
		}
	}

	for _, f := range []string{
		filepath.Join("internal", "backend", "blob.go"),
		filepath.Join("internal", "backend", "multipartform.go"),
		filepath.Join("cmd", c.BinaryName, "upload.go"),
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to be emitted: %v", f, err)
		}
	}
}

// TestNonMultipartAppEmitsNoStagingRuntime: the blob store creates directories
// and starts a GC goroutine, so an app that never uploads should not carry it.
func TestNonMultipartAppEmitsNoStagingRuntime(t *testing.T) {
	c, _ := Parse([]byte(`
id: io.pilot.plainy
app_version: 0.1.0
description: "no uploads"
namespace: plainy
backend:
  base_url: https://placeholder.invalid
methods:
  - name: plainy.ping
    summary: "ping"
    http: { verb: GET, path: /ping }
`))
	c.Resolve()
	dir := t.TempDir()
	if _, err := Generate(c, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range []string{
		filepath.Join("internal", "backend", "blob.go"),
		filepath.Join("cmd", c.BinaryName, "upload.go"),
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was emitted for an app with no multipart route", f)
		}
	}
	man, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if strings.Contains(string(man), "$APP/blobs") {
		t.Error("manifest grants $APP/blobs for an app that never stages anything")
	}
}
