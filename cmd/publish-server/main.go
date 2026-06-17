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
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/pilot-protocol/app-template/internal/publish"
)

//go:embed templates/*.html static/*
var assets embed.FS

type server struct {
	cases      *publish.CaseStore
	key        ed25519.PrivateKey
	tmpl       *template.Template
	pubToken   string
	adminToken string
	origins    []string
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
		pubToken:   os.Getenv("PILOT_PUBLISH_TOKEN"),
		adminToken: os.Getenv("ADMIN_TOKEN"),
		origins:    splitOrigins(os.Getenv("ALLOWED_ORIGINS")),
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
	mux.HandleFunc("GET /admin", s.adminList)
	mux.HandleFunc("GET /admin/case", s.adminCase)
	mux.HandleFunc("POST /admin/approve", s.adminApprove)
	mux.HandleFunc("POST /admin/reject", s.adminReject)

	log.Printf("publish-server on %s (publisher %s, origins=%v)", *addr, publish.PublisherString(key), s.origins)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// ── CORS ────────────────────────────────────────────────────────────────────

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
	bundle, err := publish.BuildBundle(sub.ToConfig(), s.key)
	if err != nil {
		writeJSON(w, 500, map[string]any{"errors": []string{"build failed: " + err.Error()}})
		return
	}
	c, err := s.cases.Create(sub, bundle, publish.BuildInfo{
		BundleName: bundle.TarballName, BundleSHA256: bundle.SHA256, Publisher: publish.PublisherString(s.key),
	})
	if err != nil {
		writeJSON(w, 409, map[string]any{"errors": []string{err.Error()}})
		return
	}
	writeJSON(w, 200, map[string]any{"case_id": c.CaseID, "status": c.Status})
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

func (s *server) adminApprove(w http.ResponseWriter, r *http.Request) {
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
	sha, perr := publish.TriggerPublish(s.cases.Dir(id), c.Submission.ID, s.pubToken)
	if perr != nil {
		s.cases.SetStatus(id, publish.StatusPending, "publish trigger failed: "+perr.Error())
	} else {
		s.cases.SetStatus(id, publish.StatusApproved, "published via workflow (commit "+sha+")")
	}
	s.redirectAdmin(w, r)
}

func (s *server) adminReject(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	reason := r.FormValue("reason")
	if reason == "" {
		reason = "rejected by reviewer"
	}
	s.cases.SetStatus(r.FormValue("id"), publish.StatusRejected, reason)
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
