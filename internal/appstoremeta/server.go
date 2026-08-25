// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// cacheSeconds is how long a consumer may serve this document without asking
// again. Presentation copy changes on a release cadence, not a request one, and
// both consumers revalidate with an ETag anyway, so a miss costs one 304.
const cacheSeconds = 300

// Server answers the read-only presentation API.
//
// The whole document is rendered once at construction and then served as fixed
// bytes: it cannot change while the process runs, so there is nothing to lock,
// nothing to invalidate, and every response is a buffer write. A deploy is how
// the data changes.
type Server struct {
	mux *http.ServeMux
}

// NewServer renders every response body up front and returns a handler.
func NewServer(document *Document) (*Server, error) {
	if document == nil {
		return nil, fmt.Errorf("appstoremeta: nil document")
	}
	// One timestamp for the life of the process: baking it into each response
	// at request time would change the bytes, and with them the ETag.
	document.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	document.SchemaVersion = SchemaVersion

	metadata, err := render(document)
	if err != nil {
		return nil, err
	}
	categories, err := render(map[string]any{
		"schema_version": SchemaVersion,
		"generated_at":   document.GeneratedAt,
		"categories":     document.Categories,
		"featured_order": document.FeaturedOrder,
	})
	if err != nil {
		return nil, err
	}
	health, err := render(map[string]any{
		"status": "ok", "apps": len(document.Apps),
		"categories": len(document.Categories), "schema_version": SchemaVersion,
		"generated_at": document.GeneratedAt,
	})
	if err != nil {
		return nil, err
	}

	byID := make(map[string]*payload, len(document.Apps))
	for _, app := range document.Apps {
		rendered, err := render(app)
		if err != nil {
			return nil, err
		}
		byID[app.ID] = rendered
	}
	known := make(map[string]bool, len(document.Categories))
	for _, category := range document.Categories {
		known[category.ID] = true
	}

	server := &Server{mux: http.NewServeMux()}
	server.mux.HandleFunc("/v1/appstore/metadata", serve(metadata))
	server.mux.HandleFunc("/v1/appstore/categories", serve(categories))
	server.mux.HandleFunc("/healthz", serve(health))
	server.mux.HandleFunc("/v1/appstore/apps", server.list(document, known))
	server.mux.HandleFunc("/v1/appstore/apps/", server.app(byID))
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// Read-only public data: any origin may read it, and no request of ours
	// carries a credential worth protecting with an origin check.
	writer.Header().Set("Access-Control-Allow-Origin", "*")
	writer.Header().Set("Vary", "Accept-Encoding")
	if request.Method == http.MethodOptions {
		writer.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		writer.Header().Set("Access-Control-Allow-Headers", "If-None-Match, Content-Type")
		writer.Header().Set("Access-Control-Max-Age", "86400")
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeJSONError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "this API is read-only")
		return
	}
	server.mux.ServeHTTP(writer, request)
}

// payload is a rendered response and the digest that lets a consumer skip it.
type payload struct {
	body []byte
	etag string
}

func render(value any) (*payload, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("appstoremeta: render: %w", err)
	}
	digest := sha256.Sum256(body)
	return &payload{body: body, etag: `"` + hex.EncodeToString(digest[:16]) + `"`}, nil
}

func serve(response *payload) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		write(writer, request, response)
	}
}

func (server *Server) list(document *Document, known map[string]bool) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		category := strings.TrimSpace(query.Get("category"))
		if category != "" && !known[category] {
			writeJSONError(writer, http.StatusBadRequest, "unknown_category",
				fmt.Sprintf("no category %q; see /v1/appstore/categories", category))
			return
		}
		search := strings.ToLower(strings.TrimSpace(query.Get("q")))
		featuredOnly := query.Get("featured") == "true"

		matches := make([]App, 0, len(document.Apps))
		for _, app := range document.Apps {
			switch {
			case category != "" && !contains(app.Categories, category):
			case featuredOnly && !app.Featured:
			case search != "" && !matchesSearch(app, search):
			default:
				matches = append(matches, app)
			}
		}

		// Filtered results are rendered per request rather than precomputed:
		// the query space is open, and the document is small enough that
		// encoding it costs less than a cache would.
		response, err := render(map[string]any{
			"schema_version": SchemaVersion,
			"generated_at":   document.GeneratedAt,
			"count":          len(matches),
			"apps":           matches,
		})
		if err != nil {
			writeJSONError(writer, http.StatusInternalServerError, "render_failed", "could not encode the result")
			return
		}
		write(writer, request, response)
	}
}

func (server *Server) app(byID map[string]*payload) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id := strings.Trim(strings.TrimPrefix(request.URL.Path, "/v1/appstore/apps/"), "/")
		response, ok := byID[id]
		if !ok {
			writeJSONError(writer, http.StatusNotFound, "app_not_found",
				fmt.Sprintf("no app %q in the presentation catalogue", id))
			return
		}
		write(writer, request, response)
	}
}

func matchesSearch(app App, needle string) bool {
	fields := make([]string, 0, 6+len(app.Keywords)+len(app.Categories))
	fields = append(fields, app.ID, app.Name, app.Tagline, app.Summary, app.Vendor)
	fields = append(fields, app.Keywords...)
	fields = append(fields, app.Categories...)
	return strings.Contains(strings.ToLower(strings.Join(fields, " ")), needle)
}

func write(writer http.ResponseWriter, request *http.Request, response *payload) {
	header := writer.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("ETag", response.etag)
	header.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
	header.Set("X-Content-Type-Options", "nosniff")

	// A consumer that already holds these bytes gets a 304 and no payload.
	if match := request.Header.Get("If-None-Match"); match != "" && etagMatches(match, response.etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	if request.Method == http.MethodHead {
		writer.WriteHeader(http.StatusOK)
		return
	}

	if acceptsGzip(request) {
		var buffer bytes.Buffer
		compressor := gzip.NewWriter(&buffer)
		if _, err := compressor.Write(response.body); err == nil && compressor.Close() == nil {
			header.Set("Content-Encoding", "gzip")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(buffer.Bytes())
			return
		}
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(response.body)
}

func acceptsGzip(request *http.Request) bool {
	for _, encoding := range strings.Split(request.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(encoding, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

// etagMatches honours the list form and the weak prefix a proxy may add.
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == etag {
			return true
		}
	}
	return false
}

func writeJSONError(writer http.ResponseWriter, status int, code, detail string) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": code, "detail": detail})
}
