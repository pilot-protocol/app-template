package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
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

	// Provision, when set, turns this into a PROVISIONED app: the broker mints a
	// per-user derived key, records identity+IP, seeds/meters a credit ledger, and
	// forwards cloud methods authenticated as that user (see provision.go). nil ⇒
	// the classic managed app (shared master key injected for everyone).
	Provision *ProvisionSpec `json:"provision,omitempty"`

	// Credit, when set on a classic managed (HTTP forward) app, gives each caller a
	// per-user spending budget in micro-dollars: the broker seeds SeedCredits on
	// first sight and debits per call; a call that would overdraw returns 402
	// (only successful 2xx calls burn credit — a failed call is refunded). This is
	// the plain-HTTP counterpart to Provision's cloud credit ledger, without key
	// minting. nil ⇒ no budget (call-count Quota still applies).
	Credit *CreditSpec `json:"credit,omitempty"`

	// Tenancy, when set, makes the broker enforce per-caller resource isolation
	// on a SHARED partner account: every resource is claimed by its creator and a
	// caller may only reference/see resources it owns (see tenancy.go). Required
	// for any app whose partner account cannot be split per user.
	Tenancy *Tenancy `json:"tenancy,omitempty"`

	// RequireAccessKey gates this app behind a shared pilot access key (see
	// accesskey.go), on top of the signed identity. Set it for any app that hands
	// out something of value (a credit grant, a phone number) to a caller who is
	// otherwise just a self-minted keypair.
	RequireAccessKey bool `json:"require_access_key"`

	master        string          // resolved from KeyEnv at load (managed: partner key; provisioned: cloud master, e.g. smk_)
	injector      AuthInjector    // built from AuthHeader/Scheme
	allowSet      map[string]bool // key = costKey(method, path); method "" = any method
	allowPatterns []allowPattern  // templated allow entries ("{x}" matches any one segment)
	allowSegs     [][]string      // every templated allow path (method-independent), for tenancy param extraction
	breaker       *Breaker

	creditSeed         int            // Credit.SeedCredits (0 ⇒ no budget)
	creditDefault      int            // Credit.DefaultCost (micro-$ per call for un-listed paths)
	creditExact        map[string]int // exact method-path → micro-$ cost
	creditPatterns     []costPattern  // templated method-path → micro-$ cost
	creditMaxPerIP     int            // Credit.MaxIdentitiesPerIP (0 ⇒ unlimited): distinct callers seedable per source IP
	creditMintCooldown time.Duration  // Credit.MintCooldownMs: per-caller re-seed touch cooldown (0 ⇒ none)
	creditRespCost     bool           // Credit.CostSource == "response": debit actual cost from the response, not a fixed pre-debit
	creditCostScale    int            // Credit.CostScale: micro-$ per unit of CostField (response mode; default 1)
	creditBalancePath  string         // Credit.BalancePath: broker answers this path from the ledger (never forwarded)

	deriveSecret     []byte          // provisioned: HMAC secret (current version) resolved from Provision.SecretEnv
	secretsByVersion map[byte][]byte // provisioned: {version: secret} accepted during a rotation grace window
	provider         CloudProvider   // provisioned: the cloud data plane (masterKeyProvider | tokenMintProvider)
}

// ProvisionSpec configures a provisioned app. It is additive: managed apps leave
// it nil and behave exactly as before.
type ProvisionSpec struct {
	Provider           string         `json:"provider"`              // "master" (default, works with a machine-scoped key) | "tokenmint" (needs an admin key)
	SecretEnv          string         `json:"secret_env"`            // env var holding the HMAC derive secret (never in this file)
	KeyVersion         int            `json:"key_version"`           // rotation tag minted onto new keys (default 1)
	SeedCredits        int            `json:"seed_credits"`          // free credits granted on first provision
	CostCredits        map[string]int `json:"cost_credits"`          // method-path → credits to debit (default 1)
	MaxIdentitiesPerIP int            `json:"max_identities_per_ip"` // per-IP distinct-caller cap (0 = unlimited)
	MintCooldownMs     int            `json:"mint_cooldown_ms"`      // per-identity re-mint cooldown (0 = none)
	ProvisionPath      string         `json:"provision_path"`        // reserved route, default "/_provision"
	BalancePath        string         `json:"balance_path"`          // reserved route, default "/_balance"
	KeyPath            string         `json:"key_path"`              // reserved: return current key, default "/_key"
	RotatePath         string         `json:"rotate_path"`           // reserved: rotate the key, default "/_rotate"
	PushPath           string         `json:"push_path"`             // cloud push route, default "/push" (debits credit)
	ListPath           string         `json:"list_path"`             // owner-scoped list route, default "/list" (free read)
	ArtifactMaxBytes   int64          `json:"artifact_max_bytes"`    // push body cap (0 = default 256MiB)
	OwnerEnvKey        string         `json:"owner_env_key"`         // machine env key stamped with the owner (default "PILOT_OWNER")
	AdminKeyEnv        string         `json:"admin_key_env"`         // tokenmint: env var holding the admin key

	// Usage metering: credit is denominated in MICRO-DOLLARS and drains by REAL
	// usage. A background loop attributes each running machine's cost
	// (resources × rate-card × elapsed) to its owner and stops the owner's
	// machines when their balance hits zero. Zero rates disable metering (the
	// flat CostCredits path still applies).
	MeterIntervalMs  int   `json:"meter_interval_ms"`   // metering tick (0 = default 60s; <0 = disabled)
	CpuHourMicros    int64 `json:"cpu_hour_micros"`     // micro-$ per cpu-hour (e.g. 43200 = $0.0432)
	MemGbHourMicros  int64 `json:"mem_gb_hour_micros"`  // micro-$ per GB-hour
	DiskGbHourMicros int64 `json:"disk_gb_hour_micros"` // micro-$ per GB-hour
}

const (
	defaultProvisionPath = "/_provision"
	defaultBalancePath   = "/_balance"
	defaultKeyPath       = "/_key"
	defaultRotatePath    = "/_rotate"
	defaultPushPath      = "/push"
	defaultListPath      = "/list"
	defaultArtifactMax   = 256 << 20
	defaultOwnerEnvKey   = "PILOT_OWNER"
	defaultCostCredits   = 1
)

// CreditSpec gives a classic managed (HTTP forward) app a per-caller spending
// budget in MICRO-DOLLARS. Each caller is seeded SeedCredits on first sight and a
// per-call cost is debited; when the balance can't cover a call the broker
// returns 402. Costs are keyed by method-path (templated paths like
// "/v1/calls/{id}" allowed, matched the same way as the allow-list); a path not
// listed costs DefaultCost.
type CreditSpec struct {
	SeedCredits int `json:"seed_credits"` // per-caller starting balance in micro-$ (e.g. 5000000 = $5)
	// CostCredits maps a method-path to the micro-$ debited per successful call.
	// A key may be method-specific ("POST /v1/numbers") or any-method ("/v1/usage");
	// paths may be templated ("/v1/calls/{id}"). This matters when one path serves
	// a free read and a paid write — e.g. GET /v1/numbers (list, free) vs
	// POST /v1/numbers (buy, $3). Method-specific keys win over any-method keys.
	CostCredits map[string]int `json:"cost_credits"`
	DefaultCost int            `json:"default_cost"` // debit for calls not matched above (default 1)

	// MaxIdentitiesPerIP caps how many DISTINCT callers may be seeded a fresh budget
	// from one source IP (0 = unlimited). This is the anti-Sybil guard that makes the
	// free per-user seed safe: once a caller depletes their budget they get 402, and
	// they cannot farm a new $5 grant by minting a fresh pilot identity from the same
	// machine — the (N+1)th distinct caller on an IP is refused (429) at first sight.
	// Source IP is read only from the broker's trusted proxy header (never client
	// X-Forwarded-For). Same enforcement as ProvisionSpec.MaxIdentitiesPerIP, applied
	// to the plain-HTTP credit path.
	MaxIdentitiesPerIP int `json:"max_identities_per_ip"`
	// MintCooldownMs is a per-caller re-touch cooldown in ms (0 = none); mirrors
	// ProvisionSpec.MintCooldownMs to slow rapid re-provision churn.
	MintCooldownMs int `json:"mint_cooldown_ms"`

	// CostSource selects how the per-call debit is computed:
	//   ""         → FIXED (default): debit CostCredits[method-path] up front, refund
	//                on a non-2xx (the classic pre-flight reservation).
	//   "response" → RESPONSE-COST: the true cost is only known from the partner
	//                response (e.g. a meta-API that returns the price of the sub-call
	//                it dispatched, incl. "dynamic" endpoints priced only after the
	//                fact). No up-front debit; instead, a call whose CostCredits entry
	//                is > 0 is treated as BILLABLE and is refused with 402 when the
	//                caller's balance is already <= 0 (free/unlisted paths always pass),
	//                and after a 2xx the ACTUAL cost read from the response (AppEntry
	//                CostField × CostScale, in micro-$) is settled against the budget,
	//                clamped so the balance floors at zero. This keeps metering true to
	//                real usage when per-endpoint prices can't be tabulated in advance.
	CostSource string `json:"cost_source"`
	// CostScale converts one unit of the response CostField into micro-dollars
	// (response mode only). E.g. a CostField reporting whole cents → 10000
	// (1¢ = $0.01 = 10000 micro-$); a field reporting dollars → 1000000. Default 1.
	CostScale int `json:"cost_scale"`

	// BalancePath, when set, is a request path the broker answers ITSELF from the
	// per-caller ledger — it is NEVER forwarded to the partner. This exists to close
	// a privacy leak: a shared-master-key app whose partner exposes an account-wide
	// "balance"/"credits" endpoint would otherwise reveal the WHOLE account's balance
	// (every pilot user's spend, pooled) to any single caller. Instead, the broker
	// returns only that caller's own remaining micro-$ budget. The partner's account
	// balance is never disclosed. The caller is seeded on first sight (same as a
	// first call), so the per-IP cap applies here too. Keep the partner's real
	// account-balance path OUT of the allow-list so it can't be forwarded.
	BalancePath string `json:"balance_path"`
}

// Safe defaults for the credit/Sybil guards. These apply when a registry omits
// the field, so a new app cannot ship with the guard silently off.
const (
	// defaultMaxIdentitiesPerIP caps distinct pilot identities that may claim a
	// budget from one source IP. Low enough to make bulk identity creation
	// costly, high enough for a shared NAT / office egress to onboard real users.
	// It is a speed bump, not a boundary: a caller with many source addresses is
	// not constrained by it, so it must never be the only control on a grant.
	defaultMaxIdentitiesPerIP = 3
)

// costPattern is a templated cost key split into segments (like allowPatterns),
// optionally scoped to one HTTP method ("" = any method).
type costPattern struct {
	method string
	segs   []string
	cost   int
}

// creditEnabled reports whether this app meters a per-caller micro-dollar budget.
// creditEnabled reports whether the per-caller budget ledger is active.
//
// It keys off the PRESENCE of the credit block, never off the seed amount, so
// that the ledger's existence and the size of a grant stay independent settings.
// `seed_credits: 0` therefore means exactly what it reads like — a zero budget,
// in which every priced path is refused with 402 — rather than "no metering".
func (a *AppEntry) creditEnabled() bool { return a.Credit != nil }

// costKey is the exact-map key for a (method, path) cost entry ("" method = any).
func costKey(method, path string) string { return method + " " + path }

// parseCostKey splits a cost_credits key into (method, path): "POST /v1/x" →
// ("POST","/v1/x"); a bare "/v1/x" (leading slash) → ("","/v1/x") = any method.
func parseCostKey(k string) (method, path string) {
	if i := strings.IndexByte(k, ' '); i > 0 && k[0] != '/' {
		return k[:i], k[i+1:]
	}
	return "", k
}

// costForCall returns the micro-$ cost of a METHOD call to path. Resolution order:
// method-specific exact → any-method exact → method-specific pattern → any-method
// pattern → DefaultCost.
func (a *AppEntry) costForCall(method, path string) int {
	if c, ok := a.creditExact[costKey(method, path)]; ok {
		return c
	}
	if c, ok := a.creditExact[costKey("", path)]; ok {
		return c
	}
	if len(a.creditPatterns) > 0 {
		segs := strings.Split(path, "/")
		anyCost, anyOK := 0, false
		for _, p := range a.creditPatterns {
			if !segmentsMatch(p.segs, segs) {
				continue
			}
			if p.method == method {
				return p.cost // method-specific wins immediately
			}
			if p.method == "" && !anyOK {
				anyCost, anyOK = p.cost, true
			}
		}
		if anyOK {
			return anyCost
		}
	}
	return a.creditDefault
}

// allowed reports whether a request path is permitted. Exact entries match
// literally (fast map hit); templated entries (containing a {name} segment, e.g.
// "/v1/calls/{call_id}") match any single non-empty segment in that position, so
// REST path params don't each need enumerating. An empty allow-list permits
// nothing (safe default; prod must declare).
// Entries are METHOD-SCOPED: "POST /v1/messages" allows only that verb, while a
// bare "/v1/messages" allows any verb on that path (backwards compatible). Method
// scoping is what lets a registry express "reads yes, writes no" — without it,
// allowing GET /v1/numbers necessarily also allows DELETE /v1/numbers/{id}.
func (a *AppEntry) allowed(method, path string) bool {
	if len(a.allowSet) == 0 && len(a.allowPatterns) == 0 {
		return false
	}
	method = strings.ToUpper(method)
	if a.allowSet[costKey(method, path)] || a.allowSet[costKey("", path)] {
		return true
	}
	segs := strings.Split(path, "/")
	for _, pat := range a.allowPatterns {
		if pat.method != "" && pat.method != method {
			continue
		}
		if segmentsMatch(pat.segs, segs) {
			return true
		}
	}
	return false
}

// allowPattern is a templated allow entry; method "" matches any verb.
type allowPattern struct {
	method string
	segs   []string
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
		a.allowSegs = nil
		for _, entry := range a.Allow {
			method, p := parseCostKey(entry) // "POST /v1/x" → ("POST","/v1/x"); "/v1/x" → ("","/v1/x")
			method = strings.ToUpper(method)
			if strings.Contains(p, "{") {
				segs := strings.Split(p, "/")
				a.allowPatterns = append(a.allowPatterns, allowPattern{method: method, segs: segs})
				a.allowSegs = append(a.allowSegs, segs)
			} else {
				a.allowSet[costKey(method, p)] = true
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

		if a.Provision != nil {
			if err := resolveProvision(a, getenv); err != nil {
				return nil, err
			}
		}
		if a.Tenancy != nil {
			if err := validateTenancy(a); err != nil {
				return nil, err
			}
			a.Tenancy.compile()
		}
		if a.Credit != nil {
			if a.Provision != nil {
				return nil, fmt.Errorf("registry: app %s: `credit` (HTTP budget) and `provision` (cloud) are mutually exclusive", a.ID)
			}
			a.creditSeed = a.Credit.SeedCredits
			// default_cost 0 is meaningful: unlisted paths are FREE (e.g. reads),
			// so only explicitly-priced method-paths debit. A negative value is a
			// typo — clamp to 0.
			a.creditDefault = a.Credit.DefaultCost
			if a.creditDefault < 0 {
				a.creditDefault = 0
			}
			// Per-IP identity cap. Defaults CLOSED: omitted means the safe default,
			// and only an explicit negative disables the guard, so switching it off is
			// always a deliberate and visible choice rather than an oversight.
			a.creditMaxPerIP = a.Credit.MaxIdentitiesPerIP
			switch {
			case a.creditMaxPerIP == 0:
				a.creditMaxPerIP = defaultMaxIdentitiesPerIP
			case a.creditMaxPerIP < 0:
				a.creditMaxPerIP = 0 // explicit opt-out: unlimited
			}
			// NOTE: mint_cooldown deliberately has NO default. Provision is touched on
			// EVERY call (not just on re-seed), and its cooldown check rejects any
			// touch inside the window — so a non-zero default would 429 every caller's
			// second call. It stays opt-in for apps that really want re-mint churn
			// control. The Sybil guard here is max_identities_per_ip, above.
			a.creditMintCooldown = time.Duration(a.Credit.MintCooldownMs) * time.Millisecond
			a.creditRespCost = a.Credit.CostSource == "response"
			a.creditCostScale = a.Credit.CostScale
			if a.creditCostScale <= 0 {
				a.creditCostScale = 1
			}
			a.creditBalancePath = a.Credit.BalancePath
			a.creditExact = map[string]int{}
			a.creditPatterns = nil
			for k, c := range a.Credit.CostCredits {
				method, path := parseCostKey(k)
				if strings.Contains(path, "{") {
					a.creditPatterns = append(a.creditPatterns, costPattern{method: method, segs: strings.Split(path, "/"), cost: c})
				} else {
					a.creditExact[costKey(method, path)] = c
				}
			}
		}
		reg.apps[a.ID] = a
	}
	return reg, nil
}

// resolveProvision fills in defaults, resolves the HMAC derive secret, and builds
// the CloudProvider for a provisioned app. Fails closed if the derive secret (or,
// for tokenmint, the admin key) is unset.
func resolveProvision(a *AppEntry, getenv func(string) string) error {
	p := a.Provision
	if p.KeyVersion <= 0 {
		p.KeyVersion = 1
	}
	if p.KeyVersion > 255 {
		return fmt.Errorf("registry: app %s: key_version must be 0-255", a.ID)
	}
	if p.ProvisionPath == "" {
		p.ProvisionPath = defaultProvisionPath
	}
	if p.KeyPath == "" {
		p.KeyPath = defaultKeyPath
	}
	if p.RotatePath == "" {
		p.RotatePath = defaultRotatePath
	}
	if p.BalancePath == "" {
		p.BalancePath = defaultBalancePath
	}
	if p.PushPath == "" {
		p.PushPath = defaultPushPath
	}
	if p.ListPath == "" {
		p.ListPath = defaultListPath
	}
	if p.ArtifactMaxBytes <= 0 {
		p.ArtifactMaxBytes = defaultArtifactMax
	}
	if p.OwnerEnvKey == "" {
		p.OwnerEnvKey = defaultOwnerEnvKey
	}
	secret := getenv(p.SecretEnv)
	if secret == "" {
		return fmt.Errorf("registry: app %s: env %s (HMAC derive secret) is empty", a.ID, p.SecretEnv)
	}
	a.deriveSecret = []byte(secret)
	a.secretsByVersion = map[byte][]byte{byte(p.KeyVersion): a.deriveSecret}

	switch p.Provider {
	case "", "master":
		// masterKeyProvider uses the cloud master key already resolved into
		// a.master (from KeyEnv) and enforces isolation broker-side via owner tags.
		a.provider = newMasterKeyProvider(a.Upstream, a.master, p.OwnerEnvKey)
	case "tokenmint":
		admin := getenv(p.AdminKeyEnv)
		if admin == "" {
			return fmt.Errorf("registry: app %s: tokenmint provider needs env %s (admin key)", a.ID, p.AdminKeyEnv)
		}
		a.provider = newTokenMintProvider(a.Upstream, admin)
	default:
		return fmt.Errorf("registry: app %s: unknown provision.provider %q", a.ID, p.Provider)
	}
	return nil
}

// AppsRequiringAccessKey lists apps whose registry entry demands an access key.
// main uses it to refuse to boot when no keys are configured, so a config
// mistake surfaces as a loud startup failure rather than a silent 401 wall.
func (r *Registry) AppsRequiringAccessKey() []string {
	var out []string
	for id, a := range r.apps {
		if a.RequireAccessKey {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
