package broker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilot-protocol/app-template/internal/pilotpath"
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
	// AccessKeys authorizes callers of apps with require_access_key. A signed
	// identity only proves possession of a self-minted key; this proves the
	// caller is an authorized pilot client at all.
	AccessKeys *AccessKeys

	reg atomic.Pointer[Registry] // hot-swappable so the registry can reload without dropping traffic

	// balanceMu/balanceBuckets back a light per-(app,caller) token-bucket rate
	// limit on the free balance-read routes (serveCreditBalance, serveBalance).
	// See allowBalanceRead.
	balanceMu      sync.Mutex
	balanceBuckets map[string]*balanceBucket
}

// New returns a Broker with sane defaults.
func New(reg *Registry, store Store) *Broker {
	b := &Broker{
		Store: store,
		// Generous ceiling for agentic partner APIs that can run for minutes.
		// The effective per-call deadline is the smaller of this and the app's
		// timeout_ms (set per app in the registry); this only stops it being the
		// surprise bottleneck.
		Client:         &http.Client{Timeout: 300 * time.Second},
		MaxBody:        8 << 20,
		balanceBuckets: map[string]*balanceBucket{},
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

// microUSD formats a micro-dollar amount as "$X.XX" (floored at zero).
func microUSD(micros int) string {
	if micros < 0 {
		micros = 0
	}
	return fmt.Sprintf("$%d.%02d", micros/1_000_000, (micros%1_000_000)/10_000)
}

// pilotBalancePath is the canonical per-user credit-balance route: every credit
// app answers it from the broker's own ledger (never forwarded), and the
// generated `<ns>.balance` adapter method dials it. Sourced from
// pilotpath.Balance so this and scaffold.BalanceMetaPath can never drift apart.
// Always available on a credit app, in addition to any creditBalancePath that
// shadows a partner's own account-balance endpoint.
const pilotBalancePath = pilotpath.Balance

// balanceBucket is a light per-(app,caller) token bucket. See allowBalanceRead.
type balanceBucket struct {
	tokens float64
	last   time.Time
}

// balanceBurst/balanceRefillPerSec size the token bucket allowBalanceRead
// enforces on the free balance-read routes. They are deliberately generous —
// this is NOT the credit-mint cooldown (a read must never be gated by that,
// see allowBalanceRead's caller) and NOT the app's billable-call Quota (a free
// meta call must never spend it); it exists only so an intentionally
// un-quota'd, pre-Admit route is not literally unbounded. A caller can always
// burst a handful of legitimate "check my balance" calls back-to-back; only a
// tight hammering loop past the burst gets shed.
const (
	balanceBurst        = 20
	balanceRefillPerSec = 5.0
)

// allowBalanceRead reports whether (app, caller) may have another balance read
// right now, consuming a token if so. Independent of ProvisionStore entirely —
// no cooldown, no Quota, no credit ledger touch — so it can never interact with
// either of those.
func (b *Broker) allowBalanceRead(app, caller string, now time.Time) bool {
	k := app + "\x00" + caller
	b.balanceMu.Lock()
	defer b.balanceMu.Unlock()
	if b.balanceBuckets == nil {
		b.balanceBuckets = map[string]*balanceBucket{}
	}
	bk := b.balanceBuckets[k]
	if bk == nil {
		bk = &balanceBucket{tokens: balanceBurst, last: now}
		b.balanceBuckets[k] = bk
	} else if elapsed := now.Sub(bk.last).Seconds(); elapsed > 0 {
		bk.tokens += elapsed * balanceRefillPerSec
		if bk.tokens > balanceBurst {
			bk.tokens = balanceBurst
		}
		bk.last = now
	}
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// serveCreditBalance answers a credit app's balance path from the per-caller
// ledger. It NEVER contacts the partner, so the shared master account's pooled
// balance is not exposed — only this caller's own remaining budget.
//
// This is a READ, not a mint: unlike the metered-call path (which touches
// ProvisionStore.Provision on every call, deliberately, to keep the caller's
// last-mint timestamp fresh), a repeat balance check must never be rejected by
// the app's mint_cooldown_ms. So an ALREADY-seeded caller is answered straight
// from ProvisionStore.Get (no cooldown check, no LastMint touch, no re-seed);
// only a caller's genuine first-ever touch goes through the normal seed path
// (still enforcing the per-IP grant cap) — and a first touch can never itself
// hit the cooldown, since there is no prior mint to be too soon after. The
// route has no Store.Admit/Quota gate (a free meta call must never spend the
// app's billable-call quota), so allowBalanceRead applies a separate, light,
// generous rate limit instead.
func (b *Broker) serveCreditBalance(w http.ResponseWriter, r *http.Request, app *AppEntry, caller string) {
	ps, ok := b.provStore()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store does not support credit metering"})
		return
	}
	now := b.now()
	if !b.allowBalanceRead(app.ID, caller, now) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "balance: rate limited — retry shortly"})
		return
	}
	rec, exists, err := ps.Get(app.ID, caller)
	if err != nil {
		b.internalError(w, http.StatusInternalServerError, app.ID, "balance", err)
		return
	}
	if !exists {
		ip := clientIP(r.Header.Get, r.RemoteAddr, b.IPTrust)
		rec, err = b.seedCaller(ps, app.ID, caller, ip, app)
		if err != nil {
			if err == ErrCooldown {
				// Unreachable in practice: nothing to cool down from on a
				// caller's first-ever touch. Handled defensively only.
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "re-provision cooldown — retry shortly"})
			} else {
				b.internalError(w, http.StatusInternalServerError, app.ID, "balance", err)
			}
			return
		}
	}
	w.Header().Set("X-Pilot-Credits-Remaining", strconv.Itoa(rec.Credits))
	writeJSON(w, http.StatusOK, map[string]any{
		"balance":           microUSD(rec.Credits),
		"credits_remaining": rec.Credits,
		"credits_seed":      app.creditSeed,
		"unit":              "micro_usd",
		"scope":             "per-pilot-user",
		"note":              "Your personal budget on this app. The shared provider account balance is not exposed.",
	})
}

// seedCaller records the caller in the credit ledger, enforcing the per-IP
// GRANT cap without ever hard-blocking access.
//
// On first sight it grants the app's seed budget, unless the caller is the
// (N+1)th distinct identity on its source IP — in which case it is recorded with
// a ZERO grant instead of being refused. The distinction matters: the cap exists
// to bound how much free budget one network can mint, not to decide who may call
// the broker. A zero-grant caller can still make free (read) calls; every priced
// call it attempts hits 402 for lack of budget. A caller already in the ledger
// keeps whatever balance it has (no re-seed, no second cap check).
//
// This is what lets the cap be set aggressively low: a tight economic bound
// costs a shared-NAT newcomer only its free grant, not its access.
func (b *Broker) seedCaller(ps ProvisionStore, app, caller, ip string, a *AppEntry) (ProvisionRecord, error) {
	rec, err := ps.Provision(app, caller, ip, a.creditSeed, a.creditMaxPerIP, a.creditMintCooldown, b.now())
	if err == ErrIPCap {
		// Over the per-IP grant cap: record the identity with no budget and no cap
		// (seed 0, maxPerIP 0). Idempotent, so a subsequent call takes the normal
		// repeat path and stays at zero rather than ever back-filling a grant.
		return ps.Provision(app, caller, ip, 0, 0, 0, b.now())
	}
	return rec, err
}

// ServeHTTP is the forward path for /<app-id>/<method-path>.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	appID, mpath, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || appID == "" {
		w.Header().Set(UnroutedHeader, "1") // not a managed-app call — monitoring skips it
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "route must be /<app-id>/<path>"})
		return
	}
	mpath = "/" + mpath

	// WHICH app first — so a provisioned push can lift the in-memory body cap to
	// the app's artifact limit before reading.
	app := b.reg.Load().Get(appID)
	if app == nil {
		w.Header().Set(UnroutedHeader, "1") // unknown app (typo / internet scanner) — not real broker usage
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app: " + appID})
		return
	}

	// Access key BEFORE anything else that costs or grants: an unauthorized caller
	// must not be able to reach the credit ledger, seed a grant, or touch the
	// master key. This is the gate that self-minted identities cannot walk past.
	if !b.requireAccessKey(w, r, app) {
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

	// 2b. Per-user balance: a credit app can answer a balance path from the broker's
	//     OWN ledger — never forwarding to the partner — so the shared account's
	//     pooled balance is never disclosed. Returns only THIS caller's remaining
	//     micro-$ budget (seeding on first sight, so the per-IP cap applies here too).
	//     Two paths reach it: the canonical /_pilot/balance (what the generated
	//     <ns>.balance method dials — always available on a credit app) and an
	//     optional creditBalancePath that SHADOWS a partner's account-balance
	//     endpoint so it can't leak the pooled account.
	if app.creditEnabled() && (mpath == pilotBalancePath || (app.creditBalancePath != "" && mpath == app.creditBalancePath)) {
		b.serveCreditBalance(w, r, app, string(caller))
		return
	}

	// 3. Is this an allowed method? (no open proxy onto the master key)
	if !app.allowed(r.Method, mpath) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "method not allowed for this app: " + mpath})
		return
	}

	// 3b. TENANCY: on a shared partner account the broker is the only thing that
	//     knows which pilot user owns what. Every resource named in the path,
	//     query, or body must belong to this caller. This runs BEFORE the forward,
	//     so a refused write never reaches the partner.
	//
	//     The answer is 404, never 403: "not yours" and "does not exist" must be
	//     indistinguishable, otherwise the broker is an oracle for enumerating
	//     other tenants' resource ids.
	if app.Tenancy != nil {
		if _, ok := app.Tenancy.EnforceRequest(b.ownerStore(), appID, app.allowSegs, r.Method, mpath, r.URL.RawQuery, body, string(caller)); !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
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
	var creditCost int // fixed mode: the amount pre-debited (refunded on failure)
	var billable bool  // response mode: this method-path costs money (CostCredits > 0)
	if app.creditEnabled() {
		var ok bool
		if ps, ok = b.provStore(); !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store does not support credit metering"})
			return
		}
		ip := clientIP(r.Header.Get, r.RemoteAddr, b.IPTrust)
		// Seed the caller on first sight, enforcing the per-IP GRANT cap. The cap
		// bounds free money, not access: the (N+1)th distinct identity on one source
		// IP is recorded with a ZERO grant rather than refused outright. It can still
		// make free (read) calls, but every priced call hits 402 — so nobody can farm
		// a fresh budget by minting identities from one network, and a legitimate user
		// behind a shared NAT is not hard-locked out of the app.
		if _, err := b.seedCaller(ps, appID, string(caller), ip, app); err != nil {
			if err == ErrCooldown {
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "re-provision cooldown — retry shortly"})
			} else {
				b.internalError(w, http.StatusInternalServerError, appID, "provision", err)
			}
			return
		}
		if app.creditRespCost {
			// RESPONSE-COST mode: no up-front debit — the true cost isn't known until
			// the partner replies. A billable path (CostCredits > 0) is only refused
			// when the caller is already depleted; free/unlisted paths always pass.
			// The actual cost is settled after a 2xx (see below).
			billable = app.costForCall(r.Method, mpath) > 0
			if billable {
				bal, derr := ps.Credit(appID, string(caller))
				if derr != nil {
					b.internalError(w, http.StatusInternalServerError, appID, "credit", derr)
					return
				}
				if bal <= 0 {
					w.Header().Set("X-Pilot-Credits-Remaining", strconv.Itoa(bal))
					writeJSON(w, http.StatusPaymentRequired, map[string]any{
						"error": "insufficient credit — per-user budget exhausted", "credits_remaining": bal,
					})
					return
				}
			}
		} else {
			// FIXED mode: pre-flight reservation of a known per-call cost.
			creditCost = app.costForCall(r.Method, mpath)
			admittedCredit, remaining, derr := ps.Debit(appID, string(caller), creditCost)
			if derr != nil {
				b.internalError(w, http.StatusInternalServerError, appID, "debit", derr)
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
	}
	// refundCredit returns this call's fixed-mode debit (used on any non-success) so
	// failures never burn budget. Response mode does no up-front debit, so it's a no-op.
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
		b.internalError(w, http.StatusBadGateway, appID, "build upstream", err)
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	app.injector.Inject(ureq, app.master)

	resp, err := b.Client.Do(ureq)
	if err != nil {
		app.breaker.Record(false)
		refundCredit()
		b.internalError(w, http.StatusBadGateway, appID, "upstream", err)
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
		// Response-cost mode: settle the ACTUAL cost the partner reported against the
		// caller's micro-dollar budget (clamped so the balance floors at zero). This is
		// what makes metering true to real usage for a meta-API whose per-call price is
		// only known from the response and can be "dynamic".
		if app.creditEnabled() && app.creditRespCost && billable {
			if micros := int(math.Round(c * float64(app.creditCostScale))); micros > 0 {
				_, _ = ps.Settle(appID, string(caller), micros)
			}
		}
	}

	// 7. TENANCY on the way back out. Two jobs, both only on success:
	//    - claim what this call created, so the caller owns it going forward;
	//    - filter list answers down to the caller's own resources, because the
	//      partner answered for the WHOLE shared account.
	if app.Tenancy != nil && resp.StatusCode/100 == 2 {
		os := b.ownerStore()
		app.Tenancy.ClaimFrom(os, appID, r.Method, mpath, rb, string(caller), b.now())
		app.Tenancy.ReleaseFrom(os, appID, app.allowSegs, r.Method, mpath)
		if filtered, did := app.Tenancy.FilterResponse(os, appID, r.Method, mpath, rb, string(caller), b.now()); did {
			rb = filtered
		} else if filtered, did := app.Tenancy.FilterObject(os, appID, r.Method, mpath, rb, string(caller)); did {
			rb = filtered
		}
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

// internalError logs the real cause server-side and returns an opaque reference
// to the caller.
//
// Raw error strings from the store or the upstream dialer describe internal
// topology — hostnames, private addresses, database paths, driver states. None
// of that helps a legitimate caller (it is never something they can fix), and
// all of it helps someone mapping the deployment. The caller gets a correlation
// id to quote in a bug report; the detail stays in the log.
func (b *Broker) internalError(w http.ResponseWriter, code int, appID, stage string, err error) {
	ref := errorRef()
	log.Printf("broker: %s: %s: ref=%s: %v", appID, stage, ref, err)
	writeJSON(w, code, map[string]string{
		"error": "the broker could not complete this call",
		"ref":   ref,
	})
}

// errorRef mints a short, non-guessable correlation id for one failure.
func errorRef() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
