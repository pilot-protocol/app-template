// Package insforgesignup is a signed HTTP broker that provisions a per-user
// InsForge backend and returns its access key — one signed call, no email, no
// browser. It is the InsForge half of the broker-signup pattern (the mailbox
// half, internal/otpmail, is NOT used here: InsForge has no email-OTP account
// signup, so instead of driving a mailbox the broker holds one master account's
// OAuth refresh token and provisions an isolated project per caller):
//
//	adapter ──POST /signup (ed25519-signed)──▶ broker
//	  broker: refresh the master OAuth token (headless)
//	          → POST create a fresh project under the master org
//	          → GET the project's access-api-key (ik_)
//	          → derive the project backend URL from appkey+region
//	          → persist {project_id, api_key, backend_url} for this caller
//	  broker ──{api_key, backend_url, project_id}──▶ adapter
//	          (adapter caches them to secrets.json; ops go DIRECT to the
//	           user's own backend with that key — the broker is off the hot path)
//
// It reuses the shared broker's ed25519 caller-identity verification, is
// idempotent per caller (a repeat /signup returns the same project, so the key
// is retrievable), and applies a per-IP cap so one caller can't farm projects.
package insforgesignup

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

// Config is the broker's deployment configuration (all from env; no account or
// host specifics compiled in).
type Config struct {
	Listen string // HTTP listen address (default 127.0.0.1:8092)

	// Master account OAuth (headless provisioning credential).
	TokenURL     string // OAuth token endpoint (e.g. https://api.insforge.dev/api/oauth/v1/token)
	ClientID     string // OAuth client id the refresh token was issued to
	RefreshToken string // master account refresh token (non-rotating)

	// Platform API + provisioning target.
	PlatformAPI   string // platform API base (e.g. https://api.insforge.dev)
	OrgID         string // org new projects are created under
	Region        string // project region (default us-east)
	BackendDomain string // backend host suffix (default insforge.app) → https://<appkey>.<region>.<domain>
	ProjectPrefix string // project name prefix (default pilot-)

	DBPath             string        // sqlite path for the per-caller ledger
	EncKeyHex          string        // 64-hex (32-byte) key encrypting the project key at rest
	MaxIdentitiesPerIP int           // per-IP distinct-caller cap (0 = unlimited)
	MintCooldown       time.Duration // min gap between mints from one IP (0 = none)

	// SignupPath is the request path the signed /signup handler is mounted at
	// (default "/signup"). Set it to the FULL public path when the broker sits
	// behind a reverse proxy that preserves the URI (e.g. "/insforge/signup"), so
	// the path the adapter signs matches the path this broker verifies.
	SignupPath string
}

// Broker orchestrates provisioning and owns the ledger.
type Broker struct {
	cfg    Config
	store  *store
	verify broker.VerifyConfig
	http   *http.Client
	log    *log.Logger

	mu     sync.Mutex
	tok    string                     // cached master access token
	tokExp time.Time                  // its expiry
	ipSeen map[string]map[string]bool // ip -> set of caller ids
	ipLast map[string]time.Time       // ip -> last mint time
}

// New validates cfg, opens the ledger, and returns a Broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8092"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east"
	}
	if cfg.BackendDomain == "" {
		cfg.BackendDomain = "insforge.app"
	}
	if cfg.ProjectPrefix == "" {
		cfg.ProjectPrefix = "pilot-"
	}
	if cfg.SignupPath == "" {
		cfg.SignupPath = "/signup"
	}
	for name, v := range map[string]string{"TokenURL": cfg.TokenURL, "ClientID": cfg.ClientID, "RefreshToken": cfg.RefreshToken, "PlatformAPI": cfg.PlatformAPI, "OrgID": cfg.OrgID} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("insforgesignup: %s is required", name)
		}
	}
	st, err := openStore(cfg.DBPath, cfg.EncKeyHex)
	if err != nil {
		return nil, err
	}
	return &Broker{
		cfg:    cfg,
		store:  st,
		http:   &http.Client{Timeout: 60 * time.Second},
		log:    log.New(io.Discard, "", 0),
		ipSeen: map[string]map[string]bool{},
		ipLast: map[string]time.Time{},
	}, nil
}

// SetLogger installs a logger (provisioning events only — never the key/token).
func (b *Broker) SetLogger(l *log.Logger) { b.log = l }

// ListenAndServe serves the broker HTTP API.
func (b *Broker) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/gw/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc(b.cfg.SignupPath, b.handleSignup)
	return http.ListenAndServe(b.cfg.Listen, mux)
}

// Account is what the broker returns to the adapter.
type Account struct {
	APIKey     string `json:"api_key"`     // the project access key (ik_)
	BackendURL string `json:"backend_url"` // the project's backend base URL
	ProjectID  string `json:"project_id"`
	Cached     bool   `json:"cached"` // true when returned from the ledger (idempotent repeat)
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
		switch {
		case errors.Is(err, errRateLimited):
			code = http.StatusTooManyRequests
		case errors.Is(err, errProjectLimit):
			code = http.StatusPaymentRequired
		}
		b.log.Printf("signup caller=%s ip=%s FAIL: %v", short(string(caller)), ip, err)
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(acct)
}

var (
	errRateLimited  = errors.New("per-IP identity cap reached (anti-abuse)")
	errProjectLimit = errors.New("the managed InsForge org has reached its project limit — the operator must upgrade the plan")
)

// Signup is the core flow, separated from HTTP so it is directly testable.
func (b *Broker) Signup(ctx context.Context, caller, ip string) (*Account, error) {
	// Idempotent: a caller that already has a project gets it back (retrievable),
	// no second project provisioned.
	if rec, ok, _ := b.store.get(caller); ok {
		b.log.Printf("signup caller=%s ip=%s cached project=%s", short(caller), ip, rec.ProjectID)
		return &Account{APIKey: rec.APIKey, BackendURL: rec.BackendURL, ProjectID: rec.ProjectID, Cached: true}, nil
	}
	if err := b.gate(caller, ip); err != nil {
		return nil, err
	}

	start := time.Now()
	token, err := b.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("master auth: %w", err)
	}
	projID, appkey, region, err := b.createProject(ctx, token)
	if err != nil {
		return nil, err
	}
	key, err := b.projectKey(ctx, token, projID)
	if err != nil {
		return nil, err
	}
	backendURL := fmt.Sprintf("https://%s.%s.%s", appkey, region, b.cfg.BackendDomain)

	if err := b.store.put(caller, record{ProjectID: projID, APIKey: key, BackendURL: backendURL, OrgID: b.cfg.OrgID, CreatedAt: time.Now().UTC()}); err != nil {
		return nil, fmt.Errorf("persist: %w", err)
	}
	b.markMinted(caller, ip)
	b.log.Printf("signup caller=%s ip=%s provisioned project=%s in %s", short(caller), ip, projID, time.Since(start).Round(time.Millisecond))
	return &Account{APIKey: key, BackendURL: backendURL, ProjectID: projID}, nil
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

// ── master auth + provisioning calls ─────────────────────────────────────────

// accessToken returns a valid master access token, refreshing (headless) when
// the cached one is missing or within 2 minutes of expiry.
func (b *Broker) accessToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	if b.tok != "" && time.Until(b.tokExp) > 2*time.Minute {
		t := b.tok
		b.mu.Unlock()
		return t, nil
	}
	b.mu.Unlock()

	st, body := b.postJSON(ctx, b.cfg.TokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": b.cfg.RefreshToken,
		"client_id":     b.cfg.ClientID,
	}, "")
	if st != 200 {
		return "", fmt.Errorf("refresh HTTP %d", st)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("refresh returned no access_token")
	}
	ttl := out.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	b.mu.Lock()
	b.tok = out.AccessToken
	b.tokExp = time.Now().Add(time.Duration(ttl) * time.Second)
	b.mu.Unlock()
	return out.AccessToken, nil
}

// createProject provisions a fresh project under the master org and returns its
// id, appkey, and region.
func (b *Broker) createProject(ctx context.Context, token string) (id, appkey, region string, err error) {
	url := b.cfg.PlatformAPI + "/organizations/v1/" + b.cfg.OrgID + "/projects"
	name := b.cfg.ProjectPrefix + randToken(10)
	st, body := b.postJSON(ctx, url, map[string]string{"name": name, "region": b.cfg.Region}, token)
	if st == 400 && strings.Contains(strings.ToLower(string(body)), "limit") {
		return "", "", "", errProjectLimit
	}
	if st != 200 && st != 201 {
		return "", "", "", fmt.Errorf("create project HTTP %d: %s", st, snippet(body))
	}
	var out struct {
		Project struct {
			ID     string `json:"id"`
			Appkey string `json:"appkey"`
			Region string `json:"region"`
		} `json:"project"`
	}
	if json.Unmarshal(body, &out) != nil || out.Project.ID == "" || out.Project.Appkey == "" {
		return "", "", "", fmt.Errorf("create project: unexpected response")
	}
	region = out.Project.Region
	if region == "" {
		region = b.cfg.Region
	}
	return out.Project.ID, out.Project.Appkey, region, nil
}

// projectKey fetches a project's access-api-key (ik_).
func (b *Broker) projectKey(ctx context.Context, token, projID string) (string, error) {
	url := b.cfg.PlatformAPI + "/projects/v1/" + projID + "/access-api-key"
	st, body := b.getJSON(ctx, url, token)
	if st != 200 {
		return "", fmt.Errorf("access-api-key HTTP %d", st)
	}
	var out struct {
		AccessAPIKey string `json:"access_api_key"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessAPIKey == "" {
		return "", fmt.Errorf("access-api-key returned no key")
	}
	return out.AccessAPIKey, nil
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

func (b *Broker) postJSON(ctx context.Context, url string, m map[string]string, token string) (int, []byte) {
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

func (b *Broker) getJSON(ctx context.Context, url, token string) (int, []byte) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Real-IP"); xf != "" {
		return xf
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
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

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
