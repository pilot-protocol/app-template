// Package otpsignup is a signed HTTP broker that mints a per-user provider API
// key WITHOUT any email input from the user. It is the orchestration half of the
// broker-signup pattern (the mailbox half is internal/otpmail):
//
//	adapter ──POST /signup (ed25519-signed)──▶ broker
//	  broker: provision pilot_<rand>@<domain> on the mail server
//	          → POST provider register {email,password}
//	          → poll the mail server for the OTP the provider emailed
//	          → POST provider verify {email,code} → api_key
//	          → persist {email,api_key} for this caller, tear the mailbox down
//	  broker ──{email, api_key}──▶ adapter (caches to secrets.json; ops stay byo)
//
// It is provider-agnostic: the register/verify URLs, the JSON path to the key,
// and the mail domain are all configuration. It reuses the shared broker's
// ed25519 caller-identity verification, and it is idempotent per caller (a
// repeat /signup returns the same persisted account, so the key is retrievable).
package otpsignup

import (
	"bytes"
	"context"
	"crypto/rand"
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

// minTokenLen is the minimum length required of the mail-control bearer token
// (see New) — a floor on shared-secret strength for the broker↔mail-server
// control plane, pending real mTLS between the two (deferred; see
// docs/BROKER-SIGNUP.md#security-notes).
const minTokenLen = 20

// Config is the broker's deployment configuration (all from env; no provider or
// host specifics compiled in).
type Config struct {
	Listen             string        // HTTP listen address (default 127.0.0.1:8090)
	MailControlURL     string        // the mail server's control-API base (internal), e.g. http://10.x:8025
	MailToken          string        // bearer token for the mail control API
	MailDomain         string        // the mail domain addresses are minted under
	AddrPrefix         string        // localpart prefix, default "pilot_"
	RegisterURL        string        // provider register endpoint
	VerifyURL          string        // provider verify-email endpoint
	KeyPath            string        // dotted path to the key in the verify response
	DBPath             string        // sqlite path for the per-caller ledger
	EncKeyHex          string        // 64-hex (32-byte) key encrypting secrets at rest
	MaxIdentitiesPerIP int           // per-IP distinct-caller cap (0 = unlimited)
	OTPTimeout         time.Duration // how long to wait for the OTP (default 90s)
	MintCooldown       time.Duration // min gap between mints from one IP (0 = none)
}

// Broker orchestrates signups and owns the ledger.
type Broker struct {
	cfg    Config
	store  *store
	verify broker.VerifyConfig
	http   *http.Client
	log    *log.Logger

	mu     sync.Mutex
	ipSeen map[string]map[string]bool // ip -> set of caller ids
	ipLast map[string]time.Time       // ip -> last mint time

	// callerLocks serializes the check-then-act in Signup per caller id, so two
	// concurrent signups for the SAME caller can't both pass the "no existing
	// account" check and both mint (and pay for) a duplicate provider account.
	// Keyed by caller so unrelated callers still proceed in parallel.
	callerLocks sync.Map // caller (string) -> *sync.Mutex
}

// New validates cfg, opens the ledger, and returns a Broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8090"
	}
	if cfg.AddrPrefix == "" {
		cfg.AddrPrefix = "pilot_"
	}
	if cfg.KeyPath == "" {
		cfg.KeyPath = "application.api_key"
	}
	if cfg.OTPTimeout <= 0 {
		cfg.OTPTimeout = 90 * time.Second
	}
	for name, v := range map[string]string{"MailControlURL": cfg.MailControlURL, "MailToken": cfg.MailToken, "MailDomain": cfg.MailDomain, "RegisterURL": cfg.RegisterURL, "VerifyURL": cfg.VerifyURL} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("otpsignup: %s is required", name)
		}
	}
	// The broker↔mail-server control channel is bearer-token auth over a
	// private interface, not mTLS (a larger deferred design change — see
	// docs/BROKER-SIGNUP.md#security-notes). Until then, enforce a floor on
	// token strength so a weak/guessable shared secret isn't the actual
	// security boundary.
	if len(cfg.MailToken) < minTokenLen {
		return nil, fmt.Errorf("otpsignup: MailToken must be at least %d characters (use a long random bearer token)", minTokenLen)
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
		ipSeen: map[string]map[string]bool{},
		ipLast: map[string]time.Time{},
	}, nil
}

// SetLogger installs a logger (signup events only — never the key/password/code).
func (b *Broker) SetLogger(l *log.Logger) { b.log = l }

// ListenAndServe serves the broker HTTP API.
func (b *Broker) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/gw/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/signup", b.handleSignup)
	return http.ListenAndServe(b.cfg.Listen, mux)
}

// Account is what the broker returns to the adapter.
type Account struct {
	Email  string `json:"email"`
	APIKey string `json:"api_key"`
	Cached bool   `json:"cached"` // true when returned from the ledger (idempotent repeat)
}

func (b *Broker) handleSignup(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	caller, err := b.verify.Verify(r.Header.Get, r.Method, r.URL.Path, body)
	if err != nil {
		http.Error(w, "identity: "+err.Error(), http.StatusUnauthorized)
		return
	}
	ip := clientIP(r)

	acct, err := b.Signup(r.Context(), string(caller), ip)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errRateLimited) {
			code = http.StatusTooManyRequests
		}
		b.log.Printf("signup caller=%s ip=%s FAIL: %v", short(string(caller)), ip, err)
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(acct)
}

var errRateLimited = errors.New("per-IP identity cap reached (anti-abuse)")

// callerLock returns (creating if needed) the mutex serializing Signup calls
// for one caller id.
func (b *Broker) callerLock(caller string) *sync.Mutex {
	v, _ := b.callerLocks.LoadOrStore(caller, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Signup is the core flow, separated from HTTP so it is directly testable.
func (b *Broker) Signup(ctx context.Context, caller, ip string) (*Account, error) {
	// Serialize per caller: without this, two concurrent requests for the same
	// caller can both observe "no existing account" below and both race through
	// to mint (and register with the provider for) a duplicate account. Locking
	// only on the caller id keeps unrelated callers running in parallel.
	lock := b.callerLock(caller)
	lock.Lock()
	defer lock.Unlock()

	// Idempotent: a caller that already has an account gets it back (retrievable),
	// no second provider account minted.
	if rec, ok, _ := b.store.get(caller); ok {
		b.log.Printf("signup caller=%s ip=%s cached email=%s", short(caller), ip, rec.Email)
		return &Account{Email: rec.Email, APIKey: rec.APIKey, Cached: true}, nil
	}
	if err := b.gate(caller, ip); err != nil {
		return nil, err
	}

	addr := b.cfg.AddrPrefix + randToken(10) + "@" + b.cfg.MailDomain
	password := randToken(16) + "Aa1!"
	start := time.Now()

	if err := b.mailControl(ctx, "/provision", addr); err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	defer b.mailControl(context.Background(), "/teardown", addr) // always tear down

	if err := b.register(ctx, addr, password); err != nil {
		return nil, err
	}
	code, err := b.pollOTP(ctx, addr)
	if err != nil {
		return nil, err
	}
	key, err := b.verifyEmail(ctx, addr, code)
	if err != nil {
		return nil, err
	}

	if err := b.store.put(caller, record{Email: addr, Password: password, APIKey: key, CreatedAt: time.Now().UTC()}); err != nil {
		return nil, fmt.Errorf("persist: %w", err)
	}
	b.markMinted(caller, ip)
	b.log.Printf("signup caller=%s ip=%s minted email=%s in %s", short(caller), ip, addr, time.Since(start).Round(time.Millisecond))
	return &Account{Email: addr, APIKey: key}, nil
}

// gate enforces the per-IP identity cap + mint cooldown.
func (b *Broker) gate(caller, ip string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.MintCooldown > 0 {
		if last, ok := b.ipLast[ip]; ok && time.Since(last) < b.cfg.MintCooldown {
			return errRateLimited
		}
	}
	if b.cfg.MaxIdentitiesPerIP > 0 {
		seen := b.ipSeen[ip]
		if seen != nil && !seen[caller] && len(seen) >= b.cfg.MaxIdentitiesPerIP {
			return errRateLimited
		}
	}
	return nil
}

func (b *Broker) markMinted(caller, ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ipSeen[ip] == nil {
		b.ipSeen[ip] = map[string]bool{}
	}
	b.ipSeen[ip][caller] = true
	b.ipLast[ip] = time.Now()
}

// ── provider + mail calls ───────────────────────────────────────────────────

func (b *Broker) register(ctx context.Context, addr, password string) error {
	st, body := b.postJSON(ctx, b.cfg.RegisterURL, map[string]string{"email": addr, "password": password})
	if st == 200 || st == 201 || st == 409 {
		return nil
	}
	if (st == 400 || st == 422) && strings.Contains(strings.ToLower(string(body)), "already") {
		return nil
	}
	if st == 429 {
		return fmt.Errorf("provider register rate-limited (429)")
	}
	return fmt.Errorf("provider register HTTP %d", st)
}

func (b *Broker) verifyEmail(ctx context.Context, addr, code string) (string, error) {
	st, body := b.postJSON(ctx, b.cfg.VerifyURL, map[string]string{"email": addr, "code": code})
	if st != 200 && st != 201 {
		return "", fmt.Errorf("provider verify HTTP %d", st)
	}
	key := digString(body, strings.Split(b.cfg.KeyPath, ".")...)
	if key == "" {
		return "", fmt.Errorf("provider verify returned no key at %q", b.cfg.KeyPath)
	}
	return key, nil
}

func (b *Broker) pollOTP(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.OTPTimeout)
	defer cancel()
	for {
		st, body := b.getJSON(ctx, b.cfg.MailControlURL+"/otp?addr="+addr)
		if st == 200 {
			var out struct {
				Ready bool   `json:"ready"`
				Code  string `json:"code"`
			}
			if json.Unmarshal(body, &out) == nil && out.Ready && out.Code != "" {
				return out.Code, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for the OTP")
		case <-time.After(3 * time.Second):
		}
	}
}

func (b *Broker) mailControl(ctx context.Context, path, addr string) error {
	st, _ := b.postJSONAuth(ctx, b.cfg.MailControlURL+path, map[string]string{"addr": addr}, b.cfg.MailToken)
	if st != 200 {
		return fmt.Errorf("mail control %s HTTP %d", path, st)
	}
	return nil
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

func (b *Broker) postJSON(ctx context.Context, url string, m map[string]string) (int, []byte) {
	return b.postJSONAuth(ctx, url, m, "")
}

func (b *Broker) postJSONAuth(ctx context.Context, url string, m map[string]string, token string) (int, []byte) {
	raw, _ := json.Marshal(m)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body
}

func (b *Broker) getJSON(ctx context.Context, url string) (int, []byte) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	if b.cfg.MailToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.MailToken)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body
}

// clientIP returns the caller's real address for the per-IP Sybil cap.
//
// SECURITY: this reads ONLY X-Real-IP (a header a client cannot set — nginx
// overwrites it with $remote_addr on the way in) or, failing that, the raw
// socket's RemoteAddr. It deliberately does NOT consult X-Forwarded-For:
// unlike X-Real-IP, XFF is a comma-separated list a client is free to prepend
// to, so trusting the client-supplied first element would let anyone forge a
// fresh "IP" per request and mint unlimited identities past MaxIdentitiesPerIP.
// See deploy/setup-broker-tls.sh for the nginx side of this trust boundary.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func digString(raw []byte, path ...string) string {
	cur := raw
	for _, p := range path {
		var m map[string]json.RawMessage
		if json.Unmarshal(cur, &m) != nil {
			return ""
		}
		nx, ok := m[p]
		if !ok {
			return ""
		}
		cur = nx
	}
	var s string
	_ = json.Unmarshal(cur, &s)
	return s
}

func randToken(n int) string {
	const a = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = a[int(b[i])%len(a)]
	}
	return string(b)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
