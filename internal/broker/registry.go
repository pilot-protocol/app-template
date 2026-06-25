package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// AppEntry is one managed app in the broker registry. Adding an app is a
// registry entry + an env var with the master key — no code per app.
type AppEntry struct {
	ID         string   `json:"id"`          // io.pilot.<name>
	Upstream   string   `json:"upstream"`    // partner API base, e.g. https://api.example.com
	KeyEnv     string   `json:"key_env"`     // env var holding the master key (never in this file)
	AuthStyle  string   `json:"auth_style"`  // "" | "header" (default) | "query" | "basic"
	AuthHeader string   `json:"auth_header"` // header style: header name, e.g. "x-api-key" | "Authorization"
	AuthScheme string   `json:"auth_scheme"` // header style: optional prefix, e.g. "Bearer"
	AuthParam  string   `json:"auth_param"`  // query style: param name, e.g. "apikey"
	AuthUser   string   `json:"auth_user"`   // basic style: username (empty = key-as-username)
	Allow      []string `json:"allow"`       // allowed method paths (e.g. "/find-email"); required in prod
	Quota      int      `json:"quota"`       // per-caller call cap (0 = unlimited)

	CostField         string `json:"cost_field"`          // dot-path to cost-in-cents in the response (default "cost_cents")
	TimeoutMs         int    `json:"timeout_ms"`          // per-call upstream timeout (0 = broker default)
	BreakerThreshold  int    `json:"breaker_threshold"`   // consecutive failures before opening (0 = disabled)
	BreakerCooldownMs int    `json:"breaker_cooldown_ms"` // how long the breaker stays open

	master        string       // resolved from KeyEnv at load
	injector      AuthInjector // built from AuthHeader/Scheme
	allowSet      map[string]bool
	allowPatterns [][]string // templated allow entries split on "/" ("{x}" matches any one segment)
	breaker       *Breaker
}

// allowed reports whether a request path is permitted. Exact entries match
// literally (fast map hit); templated entries (containing a {name} segment, e.g.
// "/v1/calls/{call_id}") match any single non-empty segment in that position, so
// REST path params don't each need enumerating. An empty allow-list permits
// nothing (safe default; prod must declare).
func (a *AppEntry) allowed(path string) bool {
	if len(a.allowSet) == 0 && len(a.allowPatterns) == 0 {
		return false
	}
	if a.allowSet[path] {
		return true
	}
	segs := strings.Split(path, "/")
	for _, pat := range a.allowPatterns {
		if segmentsMatch(pat, segs) {
			return true
		}
	}
	return false
}

// segmentsMatch reports whether request segments satisfy a templated pattern. A
// "{name}" pattern segment matches any single non-empty segment; every other
// segment must match literally. Lengths must be equal (no implicit wildcards).
func segmentsMatch(pat, segs []string) bool {
	if len(pat) != len(segs) {
		return false
	}
	for i, p := range pat {
		if len(p) >= 2 && p[0] == '{' && p[len(p)-1] == '}' {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if p != segs[i] {
			return false
		}
	}
	return true
}

// Registry holds the managed apps by id.
type Registry struct{ apps map[string]*AppEntry }

func (r *Registry) Get(id string) *AppEntry {
	if r == nil {
		return nil
	}
	return r.apps[id]
}

// LoadRegistry reads a JSON array of AppEntry, resolves each master key from its
// KeyEnv, and builds the per-app injector + allow-set. Fails if a key is unset.
//
// A missing or empty file yields an empty registry (the broker boots and serves
// 404s until apps are registered) — the publish-server writes this file on the
// first managed-app approval, then the broker reloads it on SIGHUP.
func LoadRegistry(path string, getenv func(string) string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Registry{apps: map[string]*AppEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	return ParseRegistry(raw, getenv)
}

// ParseRegistry builds a Registry from JSON bytes (split out for testing).
func ParseRegistry(raw []byte, getenv func(string) string) (*Registry, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return &Registry{apps: map[string]*AppEntry{}}, nil
	}
	var list []*AppEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("registry: parse: %w", err)
	}
	reg := &Registry{apps: map[string]*AppEntry{}}
	for _, a := range list {
		if a.ID == "" || a.Upstream == "" {
			return nil, fmt.Errorf("registry: entry missing id or upstream")
		}
		a.master = getenv(a.KeyEnv)
		if a.master == "" {
			return nil, fmt.Errorf("registry: app %s: env %s (master key) is empty", a.ID, a.KeyEnv)
		}
		a.injector = injectorFor(a.AuthStyle, a.AuthHeader, a.AuthScheme, a.AuthParam, a.AuthUser)
		a.allowSet = map[string]bool{}
		a.allowPatterns = nil
		for _, p := range a.Allow {
			if strings.Contains(p, "{") {
				a.allowPatterns = append(a.allowPatterns, strings.Split(p, "/"))
			} else {
				a.allowSet[p] = true
			}
		}
		if a.CostField == "" {
			a.CostField = "cost_cents"
		}
		cooldown := time.Duration(a.BreakerCooldownMs) * time.Millisecond
		if a.BreakerThreshold > 0 && cooldown == 0 {
			cooldown = 30 * time.Second // sane default when a threshold is set
		}
		a.breaker = &Breaker{Threshold: a.BreakerThreshold, Cooldown: cooldown}
		reg.apps[a.ID] = a
	}
	return reg, nil
}
