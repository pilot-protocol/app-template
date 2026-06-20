package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Broker is the managed-key service: verify caller → trust/quota → inject the
// master key → forward to the partner → meter. One broker fronts every managed
// app (routing by app id), so adding an app is registry config, not code.
type Broker struct {
	Store  Store
	Verify VerifyConfig
	Client *http.Client
	// MaxBody caps the request body the broker will buffer/forward.
	MaxBody int64

	reg atomic.Pointer[Registry] // hot-swappable so the registry can reload without dropping traffic
}

// New returns a Broker with sane defaults.
func New(reg *Registry, store Store) *Broker {
	b := &Broker{
		Store: store,
		// Generous ceiling for agentic partner APIs that can run for minutes.
		// The effective per-call deadline is the smaller of this and the app's
		// timeout_ms (set per app in the registry); this only stops it being the
		// surprise bottleneck.
		Client:  &http.Client{Timeout: 300 * time.Second},
		MaxBody: 8 << 20,
	}
	b.reg.Store(reg)
	return b
}

// Registry returns the currently active registry.
func (b *Broker) Registry() *Registry { return b.reg.Load() }

// SetRegistry atomically swaps in a new registry (live reload). In-flight
// requests keep using the registry they started with.
func (b *Broker) SetRegistry(reg *Registry) { b.reg.Store(reg) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ServeHTTP is the forward path for /<app-id>/<method-path>.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	appID, mpath, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || appID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "route must be /<app-id>/<path>"})
		return
	}
	mpath = "/" + mpath

	body, err := io.ReadAll(io.LimitReader(r.Body, b.MaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body"})
		return
	}

	// 1. WHO is calling — verified, not asserted. Signed over the full request.
	caller, err := b.Verify.Verify(r.Header.Get, r.Method, r.URL.Path, body)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	// 2. WHICH app.
	app := b.reg.Load().Get(appID)
	if app == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app: " + appID})
		return
	}

	// 3. Is this an allowed method? (no open proxy onto the master key)
	if !app.allowed(mpath) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "method not allowed for this app: " + mpath})
		return
	}

	// 4. Circuit breaker: if the partner has been failing, fail fast and don't
	//    spend a credit or touch the master key.
	if !app.breaker.Allow() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "upstream circuit open"})
		return
	}

	// 5. Quota (atomic check-and-count) before spending a credit.
	admitted, _ := b.Store.Admit(appID, string(caller), app.Quota)
	if !admitted {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "per-caller quota exceeded"})
		return
	}

	// 6. Forward to the partner with the master key (fresh request — caller
	//    headers are NOT carried over).
	target := app.Upstream + mpath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	ctx := r.Context()
	if app.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(app.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	ureq, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "build upstream: " + err.Error()})
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	app.injector.Inject(ureq, app.master)

	resp, err := b.Client.Do(ureq)
	if err != nil {
		app.breaker.Record(false)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	app.breaker.Record(resp.StatusCode < 500) // 5xx counts as a failure

	// 7. Meter the partner-reported cost.
	if c := extractCost(rb, app.CostField); c > 0 {
		b.Store.AddCost(appID, string(caller), c)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb)
}
