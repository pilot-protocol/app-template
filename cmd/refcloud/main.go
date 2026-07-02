// Command refcloud is a reference implementation of the smol cloud contract the
// provisioning broker's masterKeyProvider targets (see docs/CLOUD-CONTRACT.md).
// It exists so the WHOLE provisioning flow — per-user keys, credit metering, and
// broker-enforced isolation — can be exercised end-to-end in CI without spending
// real cloud credit. It mirrors the real cloud's shape deliberately:
//
//   - every call requires the master key as a Bearer token (proving the broker,
//     not the user, holds it), and
//
//   - GET /v1/machines returns ALL tenant machines with NO server-side filter, so
//     the broker's owner-tag filter is what actually enforces isolation.
//
//     REFCLOUD_MASTER=smk_… refcloud -addr 127.0.0.1:8311
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("REFCLOUD_ADDR", "127.0.0.1:8311"), "listen address")
	flag.Parse()
	master := os.Getenv("REFCLOUD_MASTER")
	if master == "" {
		log.Fatal("refcloud: REFCLOUD_MASTER (the master key the broker must present) is required")
	}

	c := &cloud{master: master}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/artifacts", c.requireMaster(c.artifacts))
	mux.HandleFunc("/v1/machines", c.requireMaster(c.machines))
	mux.HandleFunc("/v1/machines/", c.requireMaster(c.machineAction)) // <id>/start etc.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	log.Printf("refcloud: listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

type cloud struct {
	master string
	mu     sync.Mutex
	store  []map[string]any
	seq    int
}

// requireMaster rejects any call that doesn't present the master key — the whole
// point of the broker is that ONLY it can reach the cloud.
func (c *cloud) requireMaster(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+c.master {
			writeJSON(w, 401, map[string]string{"error": "missing or wrong master key"})
			return
		}
		h(w, r)
	}
}

// artifacts stores nothing durable — it just mints a reference the broker then
// uses as a machine source, echoing the owner so a namespace collision is
// impossible between users.
func (c *cloud) artifacts(w http.ResponseWriter, r *http.Request) {
	owner := r.Header.Get("X-Smol-Owner")
	_, _ = io.Copy(io.Discard, r.Body)
	writeJSON(w, 200, map[string]string{"reference": "tenants/refcloud/" + short(owner) + ":v1"})
}

func (c *cloud) machines(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		// No server-side filter — return everything (like the real cloud).
		writeJSON(w, 200, c.store)
	case http.MethodPost:
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, 422, map[string]string{"error": "bad machine body"})
			return
		}
		c.seq++
		m["id"] = fmt.Sprintf("mach-%04d", c.seq)
		m["state"] = "stopped"
		c.store = append(c.store, m)
		writeJSON(w, 201, m)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// machineAction handles /v1/machines/<id>/<action> — for the reference cloud we
// implement "start" (state → started) so a pushed VM reflects as running.
func (c *cloud) machineAction(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/machines/"), "/")
	if len(parts) < 2 {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	id, action := parts[0], parts[1]
	for _, m := range c.store {
		if m["id"] == id {
			switch action {
			case "start":
				m["state"] = "started"
			case "stop":
				m["state"] = "stopped"
			}
			writeJSON(w, 200, m)
			return
		}
	}
	writeJSON(w, 404, map[string]string{"error": "no such machine"})
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
