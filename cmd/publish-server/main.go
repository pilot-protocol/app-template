// Command publish-server is the Pilot app-store submission server: a multi-step
// wizard where a developer submits an app for review. Each "Next" POSTs the
// step and the SERVER saves the draft (no browser-side computation); the final
// review step shows everything in a table, then the server builds + signs +
// verifies the bundle, stores it pending, and on admin approval triggers the
// publish workflow.
//
// Flags / env:
//
//	-addr   listen address (default :8080)
//	-store  submission store dir (default ./store); drafts live in <store>/drafts
//	-key    platform ed25519 signing key (created if absent; default ./platform.key)
//	PILOT_PUBLISH_TOKEN  GitHub token with push to pilot-protocol/app-template
//	ADMIN_TOKEN          if set, /admin + approve/reject require ?token=<it>
package main

import (
	"crypto/ed25519"
	"embed"
	"flag"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pilot-protocol/app-template/internal/publish"
)

//go:embed templates/*.html static/*
var assets embed.FS

type server struct {
	store      *publish.Store
	drafts     *publish.DraftStore
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
	drafts, err := publish.NewDraftStore(filepath.Join(*storeDir, "drafts"))
	if err != nil {
		log.Fatalf("drafts: %v", err)
	}
	key, err := publish.LoadOrCreateKey(*keyPath)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"g":   func(v url.Values, k string) string { return v.Get(k) },
		"add": func(a, b int) int { return a + b },
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	s := &server{store: st, drafts: drafts, key: key, tmpl: tmpl,
		pubToken: os.Getenv("PILOT_PUBLISH_TOKEN"), adminToken: os.Getenv("ADMIN_TOKEN")}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /{$}", s.handleStart)
	mux.HandleFunc("GET /step/{slug}", s.handleStep)
	mux.HandleFunc("POST /step/{slug}", s.handleStepPost)
	mux.HandleFunc("GET /review", s.handleReview)
	mux.HandleFunc("POST /submit", s.handleSubmit)
	mux.HandleFunc("GET /admin", s.handleAdmin)
	mux.HandleFunc("POST /admin/approve", s.handleApprove)
	mux.HandleFunc("POST /admin/reject", s.handleReject)

	log.Printf("publish-server on %s (publisher %s)", *addr, publish.PublisherString(key))
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// handleStart mints a fresh draft and enters the wizard at step 1.
func (s *server) handleStart(w http.ResponseWriter, r *http.Request) {
	id, err := s.drafts.New()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/step/"+publish.Steps[0].Slug+"?d="+id, http.StatusSeeOther)
}

func stepIndex(slug string) (publish.Step, int, bool) {
	for i, st := range publish.Steps {
		if st.Slug == slug {
			return st, i, true
		}
	}
	return publish.Step{}, 0, false
}

func (s *server) renderStep(w http.ResponseWriter, step publish.Step, idx int, id string, draft url.Values, errs []string) {
	s.render(w, "step.html", map[string]any{
		"DraftID": id, "Step": step, "Num": idx + 1, "Total": len(publish.Steps),
		"CurIdx": idx, "Steps": publish.Steps, "Values": draft,
		"MethodRows": publish.MethodRows(draft), "HeaderRows": publish.HeaderRows(draft),
		"Durations": []string{"fast", "med", "slow"},
		"Last":      idx == len(publish.Steps)-1, "Errors": errs,
	})
}

func (s *server) handleStep(w http.ResponseWriter, r *http.Request) {
	step, idx, ok := stepIndex(r.PathValue("slug"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("d")
	draft, err := s.drafts.Load(id)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther) // unknown/expired draft → fresh start
		return
	}
	s.renderStep(w, step, idx, id, draft, nil)
}

func (s *server) handleStepPost(w http.ResponseWriter, r *http.Request) {
	step, idx, ok := stepIndex(r.PathValue("slug"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	id := r.FormValue("d")
	if err := s.drafts.MergeStep(id, step, r.PostForm); err != nil { // SAVE on every Next
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	draft, _ := s.drafts.Load(id)
	// Server-authoritative per-step validation: block advancing while invalid.
	if errs := publish.ValidateStep(step.Slug, draft); len(errs) > 0 {
		s.renderStep(w, step, idx, id, draft, errs)
		return
	}
	if idx+1 < len(publish.Steps) {
		http.Redirect(w, r, "/step/"+publish.Steps[idx+1].Slug+"?d="+id, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/review?d="+id, http.StatusSeeOther)
}

func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("d")
	draft, err := s.drafts.Load(id)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cfg, _ := publish.FormToConfig(draft)
	var problems []string
	for _, e := range cfg.Validate() {
		problems = append(problems, e.Error())
	}
	s.render(w, "review.html", map[string]any{
		"DraftID": id, "Cfg": cfg, "Steps": publish.Steps, "CurIdx": len(publish.Steps),
		"Total": len(publish.Steps), "Problems": problems,
	})
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	id := r.FormValue("d")
	draft, err := s.drafts.Load(id)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cfg, meta := publish.FormToConfig(draft)
	if errs := cfg.Validate(); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		s.render(w, "errors.html", map[string]any{"Errors": msgs, "DraftID": id})
		return
	}
	bundle, err := publish.BuildBundle(cfg, s.key)
	if err != nil {
		s.render(w, "errors.html", map[string]any{"Errors": []string{"build failed: " + err.Error()}, "DraftID": id})
		return
	}
	rec, err := s.store.Save(meta, bundle)
	if err != nil {
		s.render(w, "errors.html", map[string]any{"Errors": []string{err.Error()}, "DraftID": id})
		return
	}
	s.drafts.Delete(id)
	s.render(w, "result.html", map[string]any{"S": rec, "Publisher": publish.PublisherString(s.key)})
}

func (s *server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required (?token=…)", http.StatusUnauthorized)
		return
	}
	subs, err := s.store.List()
	if err != nil {
		http.Error(w, err.Error(), 500)
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
		http.Error(w, "unknown submission", 404)
		return
	}
	sha, err := publish.TriggerPublish(s.store.SubmissionDir(key), rec.ID, s.pubToken)
	if err != nil {
		s.store.SetStatus(key, publish.StatusPending, "publish trigger failed: "+err.Error())
	} else {
		s.store.SetStatus(key, publish.StatusApproved, "published via workflow (commit "+sha+")")
	}
	s.redirectAdmin(w, r)
}

func (s *server) handleReject(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	reason := r.FormValue("reason")
	if reason == "" {
		reason = "rejected by reviewer"
	}
	s.store.SetStatus(r.FormValue("key"), publish.StatusRejected, reason)
	s.redirectAdmin(w, r)
}

func (s *server) adminOK(r *http.Request) bool {
	if s.adminToken == "" {
		return true
	}
	return r.URL.Query().Get("token") == s.adminToken || r.FormValue("token") == s.adminToken
}

func (s *server) redirectAdmin(w http.ResponseWriter, r *http.Request) {
	u := "/admin"
	if t := r.FormValue("token"); t != "" {
		u += "?token=" + t
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}
