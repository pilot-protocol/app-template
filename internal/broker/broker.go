package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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
	// MaxBody caps the request body the broker will buffer/forward. Provisioned
	// push routes use the app's ArtifactMaxBytes instead.
	MaxBody int64
	// IPTrust says which header carries the real source IP (set by the front
	// proxy). Client-supplied X-Forwarded-For is never trusted.
	IPTrust IPTrust

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

	// WHICH app first — so a provisioned push can lift the in-memory body cap to
	// the app's artifact limit before reading.
	app := b.reg.Load().Get(appID)
	if app == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app: " + appID})
		return
	}

	maxBody := b.MaxBody
	if app.Provision != nil && mpath == app.Provision.PushPath {
		maxBody = app.Provision.ArtifactMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body"})
		return
	}

	// 1. WHO is calling — verified, not asserted. Signed over the full request.
	caller, sigErr := b.Verify.Verify(r.Header.Get, r.Method, r.URL.Path, body)

	// 2. Provisioned apps take a distinct path: mint per-user keys, meter a credit
	//    ledger, and forward through the CloudProvider (owner-tagged isolation).
	//    Signature is per-route (identity routes require it; cloud methods also
	//    accept a derived-key bearer), so verification is non-fatal here.
	if app.Provision != nil {
		b.serveProvisioned(w, r, app, mpath, caller, sigErr == nil, body)
		return
	}
	if sigErr != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": sigErr.Error()})
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

	// 5b. Credit budget (micro-dollars): seed the caller on first sight, then debit
	//     this call's cost. A call that would overdraw is refused with 402 before
	//     the master key is ever used. Only a successful (2xx) call keeps the debit;
	//     a failed call is refunded below, so users are billed for value, not errors.
	var ps ProvisionStore
	var creditCost int
	if app.creditEnabled() {
		var ok bool
		if ps, ok = b.provStore(); !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store does not support credit metering"})
			return
		}
		ip := clientIP(r.Header.Get, r.RemoteAddr, b.IPTrust)
		if _, err := ps.Provision(appID, string(caller), ip, app.creditSeed, 0, 0, b.now()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provision: " + err.Error()})
			return
		}
		creditCost = app.costForPath(mpath)
		admittedCredit, remaining, derr := ps.Debit(appID, string(caller), creditCost)
		if derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "debit: " + derr.Error()})
			return
		}
		if !admittedCredit {
			w.Header().Set("X-Pilot-Credits-Remaining", strconv.Itoa(remaining))
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error": "insufficient credit — per-user budget exhausted", "credits_remaining": remaining, "credits_required": creditCost,
			})
			return
		}
	}
	// refundCredit returns this call's debit (used on any non-success) so failures
	// never burn budget.
	refundCredit := func() {
		if app.creditEnabled() && creditCost > 0 {
			ps.Refund(appID, string(caller), creditCost)
		}
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
		refundCredit()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "build upstream: " + err.Error()})
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	app.injector.Inject(ureq, app.master)

	resp, err := b.Client.Do(ureq)
	if err != nil {
		app.breaker.Record(false)
		refundCredit()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	app.breaker.Record(resp.StatusCode < 500) // 5xx counts as a failure

	// A non-2xx call produced no value → refund its debit so only successful calls
	// burn budget. Metering (partner-reported cost) still applies on success.
	if resp.StatusCode/100 != 2 {
		refundCredit()
	} else if c := extractCost(rb, app.CostField); c > 0 {
		b.Store.AddCost(appID, string(caller), c)
	}

	// Surface the caller's remaining budget on every metered response.
	if app.creditEnabled() {
		if bal, err := ps.Credit(appID, string(caller)); err == nil {
			w.Header().Set("X-Pilot-Credits-Remaining", strconv.Itoa(bal))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb)
}
