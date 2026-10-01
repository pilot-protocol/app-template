package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPublishRichFromR2ReadsListingFields runs scripts/publish-rich-from-r2.sh
// in DRY_RUN mode against a fake R2 and checks the catalogue entry it would
// publish. Rich submissions keep their store fields under .listing; the script
// read only the top level, so republishing any rich app (plainweb 1.0.1, for
// one) replaced the live entry's categories with [], its license with "" and
// its source_url with the app-template submission path.
func TestPublishRichFromR2ReadsListingFields(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "curl", "shasum", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	const id, version = "io.pilot.listingx", "0.2.0"
	listing := map[string]any{
		"display_name": "ListingX",
		"license":      "Proprietary",
		"source_url":   "https://github.com/pilot-protocol/listingx",
		"categories":   []string{"web", "content"},
	}

	cases := []struct {
		name     string
		topLevel map[string]any // extra top-level submission fields
		want     map[string]any // expected entry fields
	}{
		{
			name: "listing only",
			want: map[string]any{
				"display_name": "ListingX",
				"license":      "Proprietary",
				"source_url":   "https://github.com/pilot-protocol/listingx",
				"categories":   []any{"web", "content"},
			},
		},
		{
			name:     "top level wins",
			topLevel: map[string]any{"license": "MIT", "categories": []string{"search"}},
			want: map[string]any{
				"display_name": "ListingX",
				"license":      "MIT",
				"source_url":   "https://github.com/pilot-protocol/listingx",
				"categories":   []any{"search"},
			},
		},
	}

	bundle := fakeRichBundle(t, `{"store":{"publisher":"ed25519:TESTONLY"}}`)
	r2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bundles/"+id+"/"+version+"/"+id+"-"+version+"-linux-amd64.tar.gz" {
			_, _ = w.Write(bundle)
			return
		}
		http.NotFound(w, r)
	}))
	defer r2.Close()

	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "publish-rich-from-r2.sh"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := map[string]any{
				"id":          id,
				"version":     version,
				"description": "A rich app with its store fields under listing.",
				"backend":     map[string]any{"type": "http", "base_url": "https://listingx.invalid"},
				"methods":     []any{map[string]any{"name": "listingx.get", "description": "get"}},
				"vendor":      map[string]any{"name": "Pilot Protocol"},
				"listing":     listing,
			}
			for k, v := range tc.topLevel {
				sub[k] = v
			}
			dir := filepath.Join(t.TempDir(), id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(sub)
			if err := os.WriteFile(filepath.Join(dir, "submission.json"), raw, 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("bash", script, dir)
			cmd.Env = append(os.Environ(),
				"DRY_RUN=1",
				"R2_PUBLIC_BASE="+r2.URL,
				"PILOT_APP_BIN="+filepath.Join(t.TempDir(), "no-pilot-app"), // skip the verify gate
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("publish-rich-from-r2.sh: %v\n%s", err, out)
			}
			entry := dryRunEntry(t, string(out))
			for k, want := range tc.want {
				if got := entry[k]; !reflect.DeepEqual(got, want) {
					t.Errorf("entry.%s = %#v, want %#v", k, got, want)
				}
			}
		})
	}
}

// dryRunEntry extracts the catalogue entry JSON the script prints in DRY_RUN.
func dryRunEntry(t *testing.T, out string) map[string]any {
	t.Helper()
	const start, end = "── DRY RUN: catalogue entry", "── DRY RUN: metadata.json"
	i := strings.Index(out, start)
	j := strings.Index(out, end)
	if i < 0 || j < i {
		t.Fatalf("no DRY RUN entry in output:\n%s", out)
	}
	body := out[i:j]
	body = body[strings.Index(body, "\n")+1:]
	var entry map[string]any
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		t.Fatalf("parse entry: %v\n%s", err, body)
	}
	return entry
}

// fakeRichBundle is a tarball holding only ./manifest.json, which is all the
// script reads from a bundle (the publisher pin) once the verify gate is off.
func fakeRichBundle(t *testing.T, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "./manifest.json", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
