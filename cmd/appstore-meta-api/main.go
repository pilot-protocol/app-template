// SPDX-License-Identifier: AGPL-3.0-or-later

// Command appstore-meta-api serves the canonical app-store presentation
// metadata read by the public website and the management console.
//
// It is deliberately tiny: the whole document is embedded in the binary,
// validated at start, and rendered once into fixed response bytes. There is no
// database, no cache to invalidate and no request-time I/O, so the process is
// stateless and a deploy is the only way its answers change.
//
//	appstore-meta-api -addr :8080
//	appstore-meta-api -data ./appstore-meta/data    # serve a working copy
//	appstore-meta-api -check                        # validate and exit
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	appstoremetadata "github.com/pilot-protocol/app-template/appstore-meta"
	"github.com/pilot-protocol/app-template/internal/appstoremeta"
)

func main() {
	addr := flag.String("addr", envOr("APPSTORE_META_ADDR", ":8080"), "listen address")
	dataDir := flag.String("data", os.Getenv("APPSTORE_META_DATA"), "serve this data directory instead of the embedded copy")
	check := flag.Bool("check", false, "validate the data and exit without listening")
	flag.Parse()

	files, source, err := dataSource(*dataDir)
	if err != nil {
		log.Fatalf("appstore-meta-api: %v", err)
	}
	document, err := appstoremeta.Load(files)
	if err != nil {
		log.Fatalf("appstore-meta-api: %v", err)
	}
	log.Printf("loaded %d apps across %d categories from %s", len(document.Apps), len(document.Categories), source)

	if *check {
		return
	}

	handler, err := appstoremeta.NewServer(document)
	if err != nil {
		log.Fatalf("appstore-meta-api: %v", err)
	}
	server := &http.Server{
		Addr:    *addr,
		Handler: logRequests(handler),
		// The payload is small and every response is already in memory, so
		// generous timeouts would only hold sockets open for a slow client.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", *addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("appstore-meta-api: listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// Drain in flight requests so a rolling restart never cuts a build's fetch
	// off mid-document.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Print("stopped")
}

// dataSource prefers an explicit directory so an author can preview an edit
// without rebuilding, and otherwise serves the copy compiled into the binary.
func dataSource(dir string) (fs.FS, string, error) {
	if dir != "" {
		return os.DirFS(dir), dir, nil
	}
	embedded, err := fs.Sub(appstoremetadata.Files, "data")
	if err != nil {
		return nil, "", err
	}
	return embedded, "the embedded snapshot", nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, request)
		log.Printf("%s %s %s %d %s", request.RemoteAddr, request.Method, request.URL.RequestURI(), recorder.status, time.Since(started).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(status int) {
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
