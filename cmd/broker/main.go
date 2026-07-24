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
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

func main() {
	registryPath := flag.String("registry", "apps.json", "path to the managed-app registry (JSON)")
	addr := flag.String("addr", envOr("BROKER_ADDR", ":8099"), "listen address")
	window := flag.Duration("window", 5*time.Minute, "signed-request freshness window")
	ipHeader := flag.String("ip-header", envOr("BROKER_IP_HEADER", "X-Real-IP"), "header carrying the real source IP (set by the front proxy; client X-Forwarded-For is never trusted)")
	meterInterval := flag.Duration("meter-interval", 60*time.Second, "usage-metering tick for provisioned apps")
	brokerName := flag.String("broker-name", envOr("BROKER_NAME", defaultHostname()), "process label on every access event")
	flag.Parse()

	reg, err := broker.LoadRegistry(*registryPath, os.Getenv)
	if err != nil {
		log.Fatalf("broker: %v", err)
	}

	// Access keys come from the environment only (never the registry file, which
	// is world-readable config and lands in git).
	accessKeys := broker.NewAccessKeys(strings.Split(os.Getenv("BROKER_ACCESS_KEYS"), ","))
	// Refuse to boot if an app demands a key that does not exist. Without this the
	// broker would come up "healthy" and 401 every caller — a config typo would
	// read as a total outage with no explanation.
	if n := reg.AppsRequiringAccessKey(); len(n) > 0 && accessKeys.Len() == 0 {
		log.Fatalf("broker: apps %v require an access key but BROKER_ACCESS_KEYS is empty — "+
			"set it (comma-separated, entries may be \"label:key\") or clear require_access_key", n)
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
	b.IPTrust = broker.IPTrust{Header: *ipHeader}
	b.AccessKeys = accessKeys
	log.Printf("broker: %d access key(s) configured", accessKeys.Len())

	// Usage meter: drain per-user credit by real machine usage, stopping machines
	// at zero. Runs for provisioned apps whose registry entry carries a rate card.
	go b.RunMeter(context.Background(), *meterInterval)

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

	// Access logging: one structured `ACCESS {json}` line per forwarded request
	// to stdout → journald (read by the monitoring log crawler). /gw/ and
	// unrouted (scanner-noise) requests are skipped.
	handler := broker.WithAccessLog(mux, *brokerName, broker.StdoutSink{W: os.Stdout}, nil)

	log.Printf("broker: listening on %s (name=%s)", *addr, *brokerName)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func defaultHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "broker"
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
