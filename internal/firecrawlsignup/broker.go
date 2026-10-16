// Package firecrawlsignup is a signed HTTP broker that provisions a per-user
// FIRECRAWL account and returns its API key — one signed call, no email input,
// no browser, no human step.
//
//	adapter ──POST /signup (ed25519-signed)──▶ broker
//	  broker: derive a stable mailbox for this Pilot identity
//	          → POST Firecrawl /partner/v1/accounts {email}
//	          → record the ToS acceptance {identity, timestamp, terms url+rev}
//	          → persist {email, api_key} for this caller (key sealed at rest)
//	  broker ──{email, api_key}──▶ adapter
//	          (adapter caches to secrets.json; every later call goes DIRECT to
//	           api.firecrawl.dev with that key — the broker is off the hot path)
//
// Why this shape rather than the shared-key proxy it replaces: Firecrawl's
// partner API gives each user their own team, so each user gets their own
// credits AND their own concurrency limit. The shared-account ceiling
// (maxConcurrency 2 for EVERY Pilot user at once) disappears, and isolation is
// enforced upstream by Firecrawl instead of by our tenancy layer.
//
// The mailbox is real and deliverable, not a throwaway: Firecrawl mails account
// notices, upgrade prompts and password resets to it, and the user must be able
// to reach them. MailDomain therefore has to be a domain we actually receive on.
//
// It reuses the shared broker's ed25519 caller-identity verification, is
// idempotent per caller (a repeat /signup returns the same account, so the key
// stays retrievable), and applies a per-IP cap so one caller cannot farm
// accounts.
package firecrawlsignup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

// DefaultTermsURL is the agreement a caller accepts by invoking signup. It is
// recorded per account so an acceptance can be tied to an exact document.
const DefaultTermsURL = "https://www.firecrawl.dev/terms-of-service"

// Config is the broker's deployment configuration (all from env; nothing
// partner- or host-specific is compiled in).
type Config struct {
	Listen string // HTTP listen address (default 127.0.0.1:8093)

	// Firecrawl partner integration.
	PartnerAPI string // partner API base (default https://integrations.firecrawl.dev)
	PartnerKey string // partner key — server-side only, NEVER in a bundle

	// Mailbox identity. Addresses are derived, never supplied by the caller, so
	// a caller cannot claim someone else's account by naming their address.
	MailDomain string // domain we receive mail on, e.g. agents.pilotprotocol.network
	AddrPrefix string // localpart prefix (default "pilot-")

	// Terms of service recorded at mint time.
	TermsURL string // default DefaultTermsURL
	TermsRev string // opaque revision tag, e.g. "2026-08"

	DBPath             string        // sqlite path for the per-caller ledger
	EncKeyHex          string        // 64-hex (32-byte) key sealing api keys at rest
	MaxIdentitiesPerIP int           // per-IP distinct-caller cap (0 = unlimited)
	MintCooldown       time.Duration // min gap between mints from one IP (0 = none)

	// SignupPath is the request path the signed handler is mounted at (default
	// "/signup"). Set it to the FULL public path when the broker sits behind a
	// reverse proxy that preserves the URI (e.g. "/firecrawl/signup"), so the
	// path the adapter signs matches the path this broker verifies.
	SignupPath string
}

// Broker provisions Firecrawl accounts and owns the ledger.
type Broker struct {
	cfg    Config
	store  *store
	verify broker.VerifyConfig
	http   *http.Client
	log    *log.Logger
	now    func() time.Time

	mu     sync.Mutex
	ipSeen map[string]map[string]bool // ip -> set of caller ids
	ipLast map[string]time.Time       // ip -> last mint time
}

// New validates cfg, opens the ledger, and returns a Broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8093"
	}
	if cfg.PartnerAPI == "" {
		cfg.PartnerAPI = "https://integrations.firecrawl.dev"
	}
	if cfg.AddrPrefix == "" {
		cfg.AddrPrefix = "pilot-"
	}
	if cfg.TermsURL == "" {
		cfg.TermsURL = DefaultTermsURL
	}
	if cfg.SignupPath == "" {
		cfg.SignupPath = "/signup"
	}
	for name, v := range map[string]string{"PartnerKey": cfg.PartnerKey, "MailDomain": cfg.MailDomain} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("firecrawlsignup: %s is required", name)
		}
	}
	st, err := openStore(cfg.DBPath, cfg.EncKeyHex)
	if err != nil {
		return nil, err
	}
	return &Broker{
		cfg:    cfg,
		store:  st,
		http:   &http.Client{Timeout: 30 * time.Second},
		log:    log.New(io.Discard, "", 0),
		now:    func() time.Time { return time.Now().UTC() },
		ipSeen: map[string]map[string]bool{},
		ipLast: map[string]time.Time{},
	}, nil
}

// SetLogger installs a logger. It logs signup events only — never the api key.
func (b *Broker) SetLogger(l *log.Logger) { b.log = l }

// SetVerify installs the caller-identity verifier.
func (b *Broker) SetVerify(v broker.VerifyConfig) { b.verify = v }

// ListenAndServe serves the broker HTTP API.
func (b *Broker) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/gw/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc(b.cfg.SignupPath, b.handleSignup)
	return http.ListenAndServe(b.cfg.Listen, mux)
}

// Account is what the broker returns to the adapter. The api key is the whole
// point of the call; everything else is context the agent can show a user.
type Account struct {
	Email      string `json:"email"`
	APIKey     string `json:"api_key"`
	Cached     bool   `json:"cached"` // true on an idempotent repeat
	TermsURL   string `json:"terms_url"`
	AcceptedAt string `json:"terms_accepted_at"`
}

var (
	errRateLimited = errors.New("per-IP identity cap reached (anti-abuse)")
	errMintCooling = errors.New("mint cooldown — retry shortly")
)

func (b *Broker) handleSignup(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	caller, err := b.verify.Verify(r.Header.Get, r.Method, r.URL.Path, body)
	if err != nil {
		http.Error(w, "identity: "+err.Error(), http.StatusUnauthorized)
		return
	}
	acct, err := b.Signup(r.Context(), string(caller), clientIP(r))
	if err != nil {
		code := http.StatusBadGateway
		switch {
		case errors.Is(err, errRateLimited), errors.Is(err, errMintCooling):
			code = http.StatusTooManyRequests
		}
		b.log.Printf("signup caller=%s FAIL: %v", short(string(caller)), err)
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(acct)
}

// Signup is the core flow, separated from HTTP so it is directly testable.
func (b *Broker) Signup(ctx context.Context, caller, ip string) (*Account, error) {
	// Idempotent: a caller that already has an account gets it back, so the key
	// stays retrievable after a reinstall and we never mint twice.
	if rec, ok, err := b.store.get(caller); err == nil && ok {
		b.log.Printf("signup caller=%s cached email=%s", short(caller), rec.Email)
		return &Account{
			Email: rec.Email, APIKey: rec.APIKey, Cached: true,
			TermsURL: rec.TermsURL, AcceptedAt: rec.AcceptedAt.Format(time.RFC3339),
		}, nil
	}
	if err := b.gate(caller, ip); err != nil {
		return nil, err
	}

	email := b.mailboxFor(caller)
	key, existed, err := b.mint(ctx, email)
	if err != nil {
		return nil, err
	}

	now := b.now()
	rec := record{
		Email: email, APIKey: key,
		TermsURL: b.cfg.TermsURL, TermsRev: b.cfg.TermsRev,
		AcceptedAt: now, CreatedAt: now,
	}
	if err := b.store.put(caller, rec); err != nil {
		return nil, fmt.Errorf("persist account: %w", err)
	}
	// A concurrent signup may have won the INSERT-OR-IGNORE; re-read so both
	// callers get the same account rather than one getting an orphan.
	if got, ok, err := b.store.get(caller); err == nil && ok {
		rec = got
	}
	b.log.Printf("signup caller=%s minted email=%s already_existed=%v", short(caller), email, existed)
	return &Account{
		Email: rec.Email, APIKey: rec.APIKey, Cached: false,
		TermsURL: rec.TermsURL, AcceptedAt: rec.AcceptedAt.Format(time.RFC3339),
	}, nil
}

// mailboxFor derives this caller's mailbox. It is a pure function of the caller
// id, so it is stable across restarts and reinstalls, and a caller can never
// name someone else's address.
func (b *Broker) mailboxFor(caller string) string {
	sum := sha256.Sum256([]byte(caller))
	return fmt.Sprintf("%s%s@%s", b.cfg.AddrPrefix, hex.EncodeToString(sum[:10]), b.cfg.MailDomain)
}

// mint calls Firecrawl's partner endpoint. alreadyExisted is returned for
// logging only: Firecrawl does not re-apply the promotional credits in that
// case, which is worth seeing in the logs when a user reports a low balance.
func (b *Broker) mint(ctx context.Context, email string) (apiKey string, alreadyExisted bool, err error) {
	payload, _ := json.Marshal(map[string]string{"email": email})
	url := strings.TrimRight(b.cfg.PartnerAPI, "/") + "/partner/v1/accounts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.PartnerKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("partner api: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Never echo the partner key; the upstream body is safe to surface.
		return "", false, fmt.Errorf("partner api: %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		APIKey         string `json:"apiKey"`
		AlreadyExisted bool   `json:"alreadyExisted"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("partner api: bad response: %w", err)
	}
	if out.APIKey == "" {
		return "", false, errors.New("partner api: response carried no apiKey")
	}
	return out.APIKey, out.AlreadyExisted, nil
}

// gate applies the per-IP distinct-caller cap and the mint cooldown.
func (b *Broker) gate(caller, ip string) error {
	if ip == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.MintCooldown > 0 {
		if last, ok := b.ipLast[ip]; ok && b.now().Sub(last) < b.cfg.MintCooldown {
			return errMintCooling
		}
	}
	if b.cfg.MaxIdentitiesPerIP > 0 {
		seen := b.ipSeen[ip]
		if seen == nil {
			seen = map[string]bool{}
			b.ipSeen[ip] = seen
		}
		if !seen[caller] && len(seen) >= b.cfg.MaxIdentitiesPerIP {
			return errRateLimited
		}
		seen[caller] = true
	}
	b.ipLast[ip] = b.now()
	return nil
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
