// Command publish-server is the Pilot app-store submission API + admin dashboard,
// hosted on the VM. The publish UI lives in the website (Astro/Cloudflare Pages)
// and calls the JSON API here (CORS-locked to the website origin). The admin
// dashboard is server-rendered here and does not move.
//
// API (CORS, JSON):
//
//	POST /api/preview  {Submission}        -> {help, commands}   live <ns>.help + pilotctl preview
//	POST /api/submit   {Submission}        -> {case_id,status} | {errors}
//
// Admin (server-rendered, token-gated):
//
//	GET  /admin                            -> case list
//	GET  /admin/case?id=<case>             -> full case report
//	POST /admin/approve  POST /admin/reject
//
// Flags / env:
//
//	-addr, -store, -key
//	PILOT_PUBLISH_TOKEN   GitHub token (approve -> publish workflow)
//	ADMIN_TOKEN           gates /admin*
//	ALLOWED_ORIGINS       comma-separated CORS origins (the website + local test)
package main

import (
	"crypto/ed25519"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pilot-protocol/app-template/internal/publish"
)

//go:embed templates/*.html static/*
var assets embed.FS

type server struct {
	cases      *publish.CaseStore
	key        ed25519.PrivateKey
	tmpl       *template.Template
	mailer     *publish.Mailer
	pubToken   string
	adminToken string
	origins    []string
	registrar  publish.BrokerRegistrar // registers managed apps with the broker on approval
	r2         *publish.R2             // artifact registry (nil = uploads disabled)
	selfBase   string                  // public base URL of THIS server, for proxy artifact URLs
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	storeDir := flag.String("store", "./store", "case store dir")
	keyPath := flag.String("key", "./platform.key", "platform ed25519 signing key (created if absent)")
	flag.Parse()

	cases, err := publish.NewCaseStore(*storeDir)
	if err != nil {
		log.Fatalf("case store: %v", err)
	}
	key, err := publish.LoadOrCreateKey(*keyPath)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"join": func(s []string) string { return strings.Join(s, ", ") },
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	s := &server{
		cases: cases, key: key, tmpl: tmpl,
		mailer:     publish.NewMailer(),
		pubToken:   os.Getenv("PILOT_PUBLISH_TOKEN"),
		adminToken: os.Getenv("ADMIN_TOKEN"),
		// CORS: only the production website may call the API. ALLOWED_ORIGINS
		// overrides (e.g. add a local origin for testing); default is prod.
		origins:  splitOrigins(allowedOriginsEnv()),
		r2:       publish.R2FromEnv(),
		selfBase: strings.TrimRight(os.Getenv("PUBLISH_SELF_URL"), "/"),
	}
	if s.r2 != nil {
		log.Printf("artifact registry: R2 bucket %q (public base %q)", s.r2.Bucket, s.r2.PublicBase)
	} else {
		log.Printf("artifact registry: disabled (set R2_ENDPOINT/R2_BUCKET + AWS keys to enable uploads)")
	}
	// Managed-app approval registers the app with the broker by writing its
	// registry file (BROKER_REGISTRY). Unset = managed registration is logged
	// for manual addition rather than written.
	if p := os.Getenv("BROKER_REGISTRY"); p != "" {
		s.registrar = publish.FileRegistrar{Path: p}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><meta charset=utf-8><title>Pilot App Store API</title>` +
			`<body style="font-family:Inter Tight,system-ui,sans-serif;max-width:560px;margin:80px auto;padding:0 24px;color:#0b0b0a">` +
			`<h1 style="font-weight:600">Pilot App Store — API</h1>` +
			`<p style="color:#5a5a54">This host serves the submission <b>API</b> (<code>/api/*</code>) and the <b>admin dashboard</b> (<code>/admin</code>). ` +
			`The publish UI lives on the website.</p></body>`))
	})
	mux.HandleFunc("/api/preview", s.cors(s.apiPreview))
	mux.HandleFunc("/api/submit", s.cors(s.apiSubmit))
	mux.HandleFunc("/api/artifact/presign", s.cors(s.apiArtifactPresign))
	// Signing proxy: install-time GET of an artifact when no public domain is set.
	// Unauthenticated by design (the daemon fetches it); R2 holds the real bytes.
	mux.HandleFunc("GET /artifact/", s.artifactProxy)
	// Self-contained admin assets (embedded). The dashboard depends on nothing
	// from the website — its CSS ships in this binary and is served from here.
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /admin", s.adminList)
	mux.HandleFunc("GET /admin/case", s.adminCase)
	mux.HandleFunc("POST /admin/build", s.adminBuild)
	mux.HandleFunc("POST /admin/approve", s.adminApprove)
	mux.HandleFunc("POST /admin/reject", s.adminReject)

	log.Printf("publish-server on %s (publisher %s, origins=%v)", *addr, publish.PublisherString(key), s.origins)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// ── CORS ────────────────────────────────────────────────────────────────────

// allowedOriginsEnv returns ALLOWED_ORIGINS, defaulting to the production
// website origins when unset so prod works without extra config.
func allowedOriginsEnv() string {
	if v := strings.TrimSpace(os.Getenv("ALLOWED_ORIGINS")); v != "" {
		return v
	}
	return "https://pilotprotocol.network,https://www.pilotprotocol.network"
}

func splitOrigins(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func (s *server) originAllowed(o string) bool {
	for _, a := range s.origins {
		if a == o || a == "*" {
			return true
		}
	}
	return false
}

// cors wraps an API handler: echoes an allowed Origin, answers preflight, and
// rejects disallowed origins. Only the website (and a local test origin) may call.
func (s *server) cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if origin != "" && !s.originAllowed(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ── API ─────────────────────────────────────────────────────────────────────

func (s *server) apiPreview(w http.ResponseWriter, r *http.Request) {
	var sub publish.Submission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	help, cmds := sub.HelpPreview()
	writeJSON(w, 200, map[string]any{"help": help, "commands": cmds})
}

func (s *server) apiSubmit(w http.ResponseWriter, r *http.Request) {
	var sub publish.Submission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	if errs := sub.Validate(); len(errs) > 0 {
		writeJSON(w, 422, map[string]any{"errors": errs})
		return
	}
	// Record the submission WITHOUT building — we don't build a bundle for every
	// submission. An admin triggers the build per case (POST /admin/build, e.g.
	// the "Build bundles" button on the case page). Returns instantly.
	c, err := s.cases.CreateSubmitted(sub)
	if err != nil {
		writeJSON(w, 409, map[string]any{"errors": []string{err.Error()}})
		return
	}
	writeJSON(w, 202, map[string]any{"case_id": c.CaseID, "status": c.Status})
}

// ── artifact registry (R2) ────────────────────────────────────────────────────

// presignReq is the website Artifacts step's request for a direct-to-R2 upload
// slot: it identifies the app + target platform + filename, and gets back a
// short-lived PUT URL plus the stable public URL to record in the submission.
type presignReq struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Filename string `json:"filename"`
}

var (
	reArtifactID   = regexp.MustCompile(`^io\.pilot\.[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	reArtifactVer  = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	reArtifactFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	okArtifactOS   = map[string]bool{"linux": true, "darwin": true}
	okArtifactArch = map[string]bool{"amd64": true, "arm64": true}
)

func (s *server) apiArtifactPresign(w http.ResponseWriter, r *http.Request) {
	if s.r2 == nil {
		writeJSON(w, 503, map[string]any{"error": "artifact uploads are not configured on this server"})
		return
	}
	var req presignReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	var errs []string
	if !reArtifactID.MatchString(req.ID) {
		errs = append(errs, "id must be io.pilot.<name>")
	}
	if !reArtifactVer.MatchString(req.Version) {
		errs = append(errs, "version must be semver")
	}
	if !okArtifactOS[req.OS] {
		errs = append(errs, "os must be linux or darwin")
	}
	if !okArtifactArch[req.Arch] {
		errs = append(errs, "arch must be amd64 or arm64")
	}
	if !reArtifactFile.MatchString(req.Filename) {
		errs = append(errs, "filename must be a plain name (letters, digits, . _ -)")
	}
	if len(errs) > 0 {
		writeJSON(w, 422, map[string]any{"errors": errs})
		return
	}
	key := publish.ArtifactKey(req.ID, req.Version, req.OS, req.Arch, req.Filename)
	putURL, err := s.r2.PresignPut(key, 15*time.Minute, time.Now())
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "presign: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"key":        key,
		"put_url":    putURL,
		"public_url": s.r2.PublicURL(key, s.selfBase),
		"expires_in": 900,
	})
}

// artifactProxy 302-redirects an install-time GET to a fresh presigned R2 GET, so
// installs work off a stable URL even when the bucket has no public domain.
func (s *server) artifactProxy(w http.ResponseWriter, r *http.Request) {
	if s.r2 == nil {
		http.Error(w, "artifact registry not configured", http.StatusServiceUnavailable)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/artifact/")
	if key == "" || strings.Contains(key, "..") {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	getURL, err := s.r2.PresignGet(key, 10*time.Minute, time.Now())
	if err != nil {
		http.Error(w, "presign: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, getURL, http.StatusFound)
}

// adminBuild kicks off the async bundle build for a submitted (or previously
// failed) case. Admin-token gated, same as approve/reject. The build runs in a
// background goroutine; the case flips submitted/build_failed → building →
// pending (or build_failed). Triggered by the "Build bundles" button on the
// case page, which injects the admin token from the dashboard URL.
func (s *server) adminBuild(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	id := r.FormValue("id")
	c, err := s.cases.Get(id)
	if err != nil {
		http.Error(w, "unknown case", 404)
		return
	}
	switch c.Status {
	case publish.StatusSubmitted, publish.StatusBuildFailed:
		// ok to (re)build
	case publish.StatusBuilding:
		http.Error(w, "already building", http.StatusConflict)
		return
	default:
		http.Error(w, fmt.Sprintf("case is %q — build only applies to submitted or build_failed cases", c.Status), http.StatusConflict)
		return
	}
	if _, err := s.cases.SetStatus(id, publish.StatusBuilding, "build started"); err != nil {
		http.Error(w, "could not start build: "+err.Error(), 500)
		return
	}
	go s.buildAsync(id, c.Submission)
	// Back to the case page (preserve the admin token), like approve/reject.
	u := "/admin/case?id=" + id
	if t := r.FormValue("token"); t != "" {
		u += "&token=" + t
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// buildAsync builds the bundle for an already-recorded case and flips it to
// pending, or marks it build_failed. Runs in its own goroutine so the submit
// response is instant (no ingress timeout on the synchronous build).
func (s *server) buildAsync(caseID string, sub publish.Submission) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("build %s panicked: %v", caseID, r)
			s.cases.SetStatus(caseID, publish.StatusBuildFailed, fmt.Sprintf("build panicked: %v", r))
		}
	}()
	bundle, err := publish.BuildBundle(sub.ToConfig(), s.key)
	if err != nil {
		log.Printf("build %s failed: %v", caseID, err)
		s.cases.SetStatus(caseID, publish.StatusBuildFailed, "build failed: "+err.Error())
		return
	}
	if _, err := s.cases.AttachBundle(caseID, bundle, publish.BuildInfo{
		BundleName: bundle.TarballName, BundleSHA256: bundle.SHA256, Publisher: publish.PublisherString(s.key),
	}); err != nil {
		log.Printf("attach bundle %s failed: %v", caseID, err)
		s.cases.SetStatus(caseID, publish.StatusBuildFailed, "store bundle failed: "+err.Error())
		return
	}
	// Confirmation email (best-effort; the build already succeeded).
	subject, htmlBody, text := publish.ConfirmationEmail(sub)
	if err := s.mailer.Send(sub.Email, subject, htmlBody, text); err != nil {
		log.Printf("confirmation email to %s failed: %v", sub.Email, err)
	}
}

// ── admin (server-rendered, stays on the VM) ──────────────────────────────────

func (s *server) adminOK(r *http.Request) bool {
	if s.adminToken == "" {
		return true
	}
	return r.URL.Query().Get("token") == s.adminToken || r.FormValue("token") == s.adminToken
}

func (s *server) adminList(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required (?token=…)", http.StatusUnauthorized)
		return
	}
	list, err := s.cases.List()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, "admin.html", map[string]any{"Cases": list, "Token": r.URL.Query().Get("token")})
}

func (s *server) adminCase(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	c, err := s.cases.Get(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "unknown case", 404)
		return
	}
	help, cmds := c.Submission.HelpPreview()
	s.render(w, "case.html", map[string]any{"C": c, "Help": help, "Commands": cmds, "Token": r.URL.Query().Get("token")})
}

// registerManaged registers a managed app with the broker (so it routes +
// meters). No-op for BYO apps. Ops must set the master key in the env var named
// by the entry (logged) before the app is callable.
func (s *server) registerManaged(sub publish.Submission) {
	if !sub.Backend.Managed() {
		return
	}
	entry := sub.BrokerEntry()
	if s.registrar == nil {
		log.Printf("broker: managed app %s approved but BROKER_REGISTRY unset; add manually: upstream=%s key_env=%s allow=%v",
			entry.ID, entry.Upstream, entry.KeyEnv, entry.Allow)
		return
	}
	if err := s.registrar.Register(entry); err != nil {
		log.Printf("broker registration for %s failed: %v", entry.ID, err)
		return
	}
	log.Printf("broker: registered managed app %s -> %s (set master key in env %s; HUP the broker to load)",
		entry.ID, entry.Upstream, entry.KeyEnv)
}

func (s *server) adminApprove(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	id := r.FormValue("id")
	guide := strings.TrimSpace(r.FormValue("guide"))
	if guide == "" {
		http.Error(w, "a 'how to find your app in the store' guide is required to approve", http.StatusBadRequest)
		return
	}
	c, err := s.cases.Get(id)
	if err != nil {
		http.Error(w, "unknown case", 404)
		return
	}
	if c.Status != publish.StatusPending {
		http.Error(w, fmt.Sprintf("case is %q, not pending — wait for the build to finish (build_failed ⇒ re-submit)", c.Status), http.StatusConflict)
		return
	}

	// Managed apps become routable on approval: register with the broker FIRST,
	// independent of the catalog publish (which only makes the app discoverable).
	// So a transient publish failure can't leave an approved app unusable.
	s.registerManaged(c.Submission)

	prURL, perr := publish.TriggerPublish(s.cases.Dir(id), c.Submission.ID, s.pubToken)
	if perr != nil {
		s.cases.SetStatus(id, publish.StatusPending, "broker-registered; catalog PR failed (retry): "+perr.Error())
		s.redirectAdmin(w, r)
		return
	}
	s.cases.SetStatus(id, publish.StatusApproved, "approved + broker-registered; catalog PR opened: "+prURL)

	subject, htmlBody, text := publish.AcceptEmail(c.Submission, guide)
	if err := s.mailer.Send(c.Submission.Email, subject, htmlBody, text); err != nil {
		log.Printf("accept email to %s failed: %v", c.Submission.Email, err)
	}
	s.redirectAdmin(w, r)
}

func (s *server) adminReject(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	id := r.FormValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		http.Error(w, "a justification is required to reject", http.StatusBadRequest)
		return
	}
	c, err := s.cases.Get(id)
	if err != nil {
		http.Error(w, "unknown case", 404)
		return
	}
	s.cases.SetStatus(id, publish.StatusRejected, reason)
	subject, htmlBody, text := publish.RejectEmail(c.Submission, reason)
	if err := s.mailer.Send(c.Submission.Email, subject, htmlBody, text); err != nil {
		log.Printf("reject email to %s failed: %v", c.Submission.Email, err)
	}
	s.redirectAdmin(w, r)
}

func (s *server) redirectAdmin(w http.ResponseWriter, r *http.Request) {
	u := "/admin"
	if t := r.FormValue("token"); t != "" {
		u += "?token=" + t
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}
