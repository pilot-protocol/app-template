// Command publish-server is the Pilot app-store submission server: a small
// web app where a developer submits an app for review via a form, the SERVER
// builds + signs + verifies the bundle (no browser-side computation), stores it
// pending, and on admin approval triggers the publish workflow.
//
// Flags / env:
//
//	-addr          listen address (default :8080)
//	-store         submission store dir (default ./store)
//	-key           platform ed25519 signing key (created if absent; default ./platform.key)
//	PILOT_PUBLISH_TOKEN   GitHub token with push to pilot-protocol/app-template (for approval)
//	ADMIN_TOKEN           if set, /admin + approve/reject require ?token=<it>
package main

import (
	"crypto/ed25519"
	"embed"
	"flag"
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/pilot-protocol/app-template/internal/publish"
)

//go:embed templates/*.html static/*
var assets embed.FS

type server struct {
	store      *publish.Store
	key        ed25519.PrivateKey
	tmpl       *template.Template
	pubToken   string
	adminToken string
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	storeDir := flag.String("store", "./store", "submission store dir")
	keyPath := flag.String("key", "./platform.key", "platform ed25519 signing key (created if absent)")
	flag.Parse()

	st, err := publish.NewStore(*storeDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	key, err := publish.LoadOrCreateKey(*keyPath)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	tmpl, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	s := &server{
		store: st, key: key, tmpl: tmpl,
		pubToken:   os.Getenv("PILOT_PUBLISH_TOKEN"),
		adminToken: os.Getenv("ADMIN_TOKEN"),
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("/", s.handleForm)
	mux.HandleFunc("/submit", s.handleSubmit)
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/approve", s.handleApprove)
	mux.HandleFunc("/admin/reject", s.handleReject)

	log.Printf("publish-server on %s (publisher %s)", *addr, publish.PublisherString(key))
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *server) handleForm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.render(w, "submit.html", map[string]any{"Durations": []string{"fast", "med", "slow"}})
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, "bad form: "+err.Error())
		return
	}
	cfg, meta := publish.FormToConfig(r.PostForm)
	if errs := cfg.Validate(); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		s.render(w, "errors.html", map[string]any{"Errors": msgs})
		return
	}
	bundle, err := publish.BuildBundle(cfg, s.key)
	if err != nil {
		s.render(w, "errors.html", map[string]any{"Errors": []string{"build failed: " + err.Error()}})
		return
	}
	rec, err := s.store.Save(meta, bundle)
	if err != nil {
		s.fail(w, err.Error())
		return
	}
	s.render(w, "result.html", map[string]any{"S": rec, "Publisher": publish.PublisherString(s.key)})
}

func (s *server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required (?token=…)", http.StatusUnauthorized)
		return
	}
	subs, err := s.store.List()
	if err != nil {
		s.fail(w, err.Error())
		return
	}
	s.render(w, "admin.html", map[string]any{"Subs": subs, "Token": r.URL.Query().Get("token")})
}

func (s *server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	key := r.FormValue("key")
	rec, err := s.store.Get(key)
	if err != nil {
		s.fail(w, "unknown submission: "+key)
		return
	}
	sha, err := publish.TriggerPublish(s.store.SubmissionDir(key), rec.ID, s.pubToken)
	if err != nil {
		s.store.SetStatus(key, publish.StatusPending, "publish trigger failed: "+err.Error())
		s.fail(w, "publish trigger failed: "+err.Error())
		return
	}
	s.store.SetStatus(key, publish.StatusApproved, "published via workflow (commit "+sha+")")
	s.redirectAdmin(w, r)
}

func (s *server) handleReject(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	key := r.FormValue("key")
	reason := r.FormValue("reason")
	if reason == "" {
		reason = "rejected by reviewer"
	}
	if _, err := s.store.SetStatus(key, publish.StatusRejected, reason); err != nil {
		s.fail(w, err.Error())
		return
	}
	s.redirectAdmin(w, r)
}

func (s *server) adminOK(r *http.Request) bool {
	if s.adminToken == "" {
		return true // no gate configured (dev)
	}
	return r.URL.Query().Get("token") == s.adminToken || r.FormValue("token") == s.adminToken
}

func (s *server) redirectAdmin(w http.ResponseWriter, r *http.Request) {
	t := r.FormValue("token")
	url := "/admin"
	if t != "" {
		url += "?token=" + t
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (s *server) fail(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, "errors.html", map[string]any{"Errors": []string{msg}})
}
