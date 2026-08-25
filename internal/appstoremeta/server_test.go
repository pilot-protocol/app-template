// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) http.Handler {
	t.Helper()
	second := strings.NewReplacer(
		`"io.pilot.postgres"`, `"io.pilot.aegis"`,
		`"PostgreSQL"`, `"AEGIS"`,
		`"Run and query PostgreSQL from an agent"`, `"A firewall for agent inputs"`,
		`# PostgreSQL\n\nThis app installs the **official** toolchain.`, `# AEGIS\n\nBlocks **prompt injection** before it lands.`,
		`"data"`, `"security"`,
		`"featured": true`, `"featured": false`,
		`postgres.query`, `aegis.scan`,
	).Replace(validApp)

	index := `{
	  "schema_version": 1,
	  "source": "test",
	  "categories": [
	    {"id":"data","name":"Data & Storage","blurb":"Databases.","hue":125},
	    {"id":"security","name":"Security","blurb":"Guardrails.","hue":5}
	  ],
	  "featured_order": ["io.pilot.postgres"]
	}`

	document, err := Load(fsWith(index, map[string]string{"io.pilot.postgres": validApp, "io.pilot.aegis": second}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	handler, err := NewServer(document)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return handler
}

func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	return value
}

func TestMetadataServesTheWholeDocument(t *testing.T) {
	recorder := get(t, testServer(t), "/v1/appstore/metadata")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Errorf("content type %q", contentType)
	}
	document := decode[Document](t, recorder)
	if len(document.Apps) != 2 {
		t.Errorf("want 2 apps, got %d", len(document.Apps))
	}
	if len(document.Categories) != 2 {
		t.Errorf("want 2 categories, got %d", len(document.Categories))
	}
	if document.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version %d", document.SchemaVersion)
	}
	if document.GeneratedAt == "" {
		t.Error("generated_at is empty")
	}
}

func TestMetadataIsCacheableAndRevalidates(t *testing.T) {
	handler := testServer(t)
	first := get(t, handler, "/v1/appstore/metadata")

	etag := first.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("want a quoted ETag, got %q", etag)
	}
	if cache := first.Header().Get("Cache-Control"); !strings.Contains(cache, "max-age=") {
		t.Errorf("want a max-age, got %q", cache)
	}

	// A consumer polling for changes must get a cheap 304, not the payload.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/appstore/metadata", nil)
	request.Header.Set("If-None-Match", etag)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes", recorder.Body.Len())
	}
	if recorder.Header().Get("ETag") != etag {
		t.Error("304 must repeat the ETag")
	}
}

func TestETagIsStableAcrossRequests(t *testing.T) {
	handler := testServer(t)
	if first, second := get(t, handler, "/v1/appstore/metadata"), get(t, handler, "/v1/appstore/metadata"); first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Error("ETag changed between two identical requests")
	}
}

func TestAppsListFilters(t *testing.T) {
	handler := testServer(t)

	type list struct {
		Count int   `json:"count"`
		Apps  []App `json:"apps"`
	}

	all := decode[list](t, get(t, handler, "/v1/appstore/apps"))
	if all.Count != 2 || len(all.Apps) != 2 {
		t.Fatalf("unfiltered list: count=%d len=%d", all.Count, len(all.Apps))
	}

	byCategory := decode[list](t, get(t, handler, "/v1/appstore/apps?category=security"))
	if byCategory.Count != 1 || byCategory.Apps[0].ID != "io.pilot.aegis" {
		t.Errorf("category filter: %+v", byCategory)
	}

	featured := decode[list](t, get(t, handler, "/v1/appstore/apps?featured=true"))
	if featured.Count != 1 || featured.Apps[0].ID != "io.pilot.postgres" {
		t.Errorf("featured filter: %+v", featured)
	}

	// Search covers name, tagline, keywords and copy — an operator types what
	// the app does, not its reverse-DNS id.
	search := decode[list](t, get(t, handler, "/v1/appstore/apps?q=POSTGRE"))
	if search.Count != 1 || search.Apps[0].ID != "io.pilot.postgres" {
		t.Errorf("query filter: %+v", search)
	}

	if empty := decode[list](t, get(t, handler, "/v1/appstore/apps?q=nothingmatchesthis")); empty.Count != 0 {
		t.Errorf("want an empty result, got %d", empty.Count)
	}
}

func TestAppsListRejectsAnUnknownCategory(t *testing.T) {
	// Silently returning everything would look like a working filter.
	recorder := get(t, testServer(t), "/v1/appstore/apps?category=ghost")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", recorder.Code)
	}
}

func TestSingleApp(t *testing.T) {
	handler := testServer(t)

	recorder := get(t, handler, "/v1/appstore/apps/io.pilot.postgres")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	app := decode[App](t, recorder)
	if app.ID != "io.pilot.postgres" || app.Name != "PostgreSQL" {
		t.Errorf("wrong app: %+v", app)
	}
	if app.Summary == "" {
		t.Error("the derived summary must be served")
	}

	missing := get(t, handler, "/v1/appstore/apps/io.pilot.ghost")
	if missing.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", missing.Code)
	}
	if !strings.Contains(missing.Body.String(), "app_not_found") {
		t.Errorf("404 body: %s", missing.Body.String())
	}
}

func TestCategories(t *testing.T) {
	recorder := get(t, testServer(t), "/v1/appstore/categories")

	body := decode[struct {
		Categories []Category `json:"categories"`
	}](t, recorder)
	if len(body.Categories) != 2 || body.Categories[0].ID != "data" {
		t.Errorf("categories: %+v", body.Categories)
	}
}

func TestHealth(t *testing.T) {
	recorder := get(t, testServer(t), "/healthz")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	body := decode[struct {
		Status string `json:"status"`
		Apps   int    `json:"apps"`
	}](t, recorder)
	if body.Status != "ok" || body.Apps != 2 {
		t.Errorf("health: %+v", body)
	}
}

func TestCrossOriginReadsAreAllowed(t *testing.T) {
	// The public site builds from a different origin, and a browser preflight
	// must not need a code change to succeed.
	handler := testServer(t)
	recorder := get(t, handler, "/v1/appstore/metadata")
	if origin := recorder.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("allow-origin %q", origin)
	}

	preflight := httptest.NewRecorder()
	handler.ServeHTTP(preflight, httptest.NewRequest(http.MethodOptions, "/v1/appstore/metadata", nil))
	if preflight.Code != http.StatusNoContent {
		t.Errorf("preflight status %d", preflight.Code)
	}
}

func TestWritesAreRejected(t *testing.T) {
	// The document is read-only. A write reaching a handler at all would be a
	// bug worth failing loudly.
	handler := testServer(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/appstore/metadata", strings.NewReader("{}")))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("405 must advertise Allow, got %q", allow)
	}
}

func TestHeadCarriesHeadersButNoBody(t *testing.T) {
	handler := testServer(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/v1/appstore/metadata", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes", recorder.Body.Len())
	}
	if recorder.Header().Get("ETag") == "" {
		t.Error("HEAD must carry the ETag")
	}
}

func TestGzipWhenAsked(t *testing.T) {
	handler := testServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/appstore/metadata", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(recorder, request)

	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "gzip" {
		t.Fatalf("content-encoding %q", encoding)
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var document Document
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(document.Apps) != 2 {
		t.Errorf("gzipped document has %d apps", len(document.Apps))
	}
}

func TestUnknownPathIs404(t *testing.T) {
	if recorder := get(t, testServer(t), "/v1/appstore/nope"); recorder.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", recorder.Code)
	}
}
