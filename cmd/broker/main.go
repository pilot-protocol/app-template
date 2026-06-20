// Command broker is the managed-key gateway. It holds the partner master keys,
// verifies who is calling (signed ed25519 identity), meters per caller, and
// forwards to the partner API. One broker fronts every managed app; adding an
// app is a registry entry + an env var, not code.
//
// Usage:
//
//	BROKER_ADDR=:8099 \
//	PARTNER_KEY=sk-... \
//	  broker -registry ./apps.json
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

func main() {
	registryPath := flag.String("registry", "apps.json", "path to the managed-app registry (JSON)")
	addr := flag.String("addr", envOr("BROKER_ADDR", ":8099"), "listen address")
	window := flag.Duration("window", 5*time.Minute, "signed-request freshness window")
	flag.Parse()

	reg, err := broker.LoadRegistry(*registryPath, os.Getenv)
	if err != nil {
		log.Fatalf("broker: %v", err)
	}

	// Durable store when BROKER_DB is set (prod); in-memory otherwise (dev).
	var store interface {
		broker.Store
		Snapshot() map[string]struct {
			Calls int     `json:"calls"`
			Cents float64 `json:"cents"`
		}
	}
	if dbPath := os.Getenv("BROKER_DB"); dbPath != "" {
		s, err := broker.OpenSQLiteStore(dbPath)
		if err != nil {
			log.Fatalf("broker: open store: %v", err)
		}
		defer s.Close()
		store = s
		log.Printf("broker: durable store at %s", dbPath)
	} else {
		store = broker.NewMemStore()
		log.Printf("broker: in-memory store (set BROKER_DB for durability)")
	}

	b := broker.New(reg, store)
	b.Verify = broker.VerifyConfig{Window: *window}

	// Hot reload: `kill -HUP <pid>` re-reads the registry without dropping
	// traffic, so a new app goes live without a restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			next, err := broker.LoadRegistry(*registryPath, os.Getenv)
			if err != nil {
				log.Printf("broker: reload failed, keeping current registry: %v", err)
				continue
			}
			b.SetRegistry(next)
			log.Printf("broker: registry reloaded from %s", *registryPath)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/gw/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/gw/usage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, store.Snapshot())
	})
	mux.Handle("/", b)

	log.Printf("broker: listening on %s", *addr)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
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
