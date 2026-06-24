// Package scaffold turns a declarative pilot.app.yaml spec into a complete,
// buildable Pilot Protocol app-store adapter project — the same shape as the
// hand-written reference app io.pilot.cosift, but generated from config.
//
// The generated project is a thin, stateless adapter: it speaks the app-store
// IPC protocol on the unix socket the daemon hands it, and forwards each method
// to a backend (an HTTP API today; a local CLI under the planned proc.exec
// capability). All heavy state lives in that backend — the adapter is boilerplate.
package scaffold

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultAppStoreModule pins the published app-store module the generated
// adapter imports for pkg/ipc. Overridable per-spec via app_store_module.
// Matches the pin the reference app (cosift-app) ships today.
const defaultAppStoreModule = "github.com/pilot-protocol/app-store v1.0.1-beta.1.0.20260622180016-07b4170265dc"

// Config is the full pilot.app.yaml spec. Only id, app_version, description,
// backend, and methods are required from the author; everything else is
// derived in Resolve.
type Config struct {
	ID              string `yaml:"id"`               // reverse-DNS, e.g. io.pilot.weather
	AppVersion      string `yaml:"app_version"`      // semver; bump every build
	ManifestVersion int    `yaml:"manifest_version"` // bump only when grants change
	Description     string `yaml:"description"`

	Namespace      string `yaml:"namespace"`        // method prefix; default = last id segment
	BinaryName     string `yaml:"binary_name"`      // compiled binary; default <namespace>-app
	GoModule       string `yaml:"go_module"`        // module path of the generated repo
	AppStoreModule string `yaml:"app_store_module"` // override the pinned app-store require line

	Publisher Publisher `yaml:"publisher"`
	Backend   Backend   `yaml:"backend"`
	Methods   []Method  `yaml:"methods"`
	Grants    Grants    `yaml:"grants"`
	Listing   Listing   `yaml:"listing"` // store-page metadata (catalogue v2)

	// Assets is the native-binary delivery set for a cli backend: the
	// platform-specific binaries the publisher uploaded to the Pilot R2 artifact
	// registry. At install the generated adapter fetches the asset matching the
	// host os/arch, verifies its sha256, stages it under $APP/<exec_path>, and (in
	// `order`) runs any with install `args`. The fronted command then execs the
	// staged path instead of an assumed-installed binary. Empty for http apps and
	// for cli apps whose command is already present on the host. See
	// docs/R2-ARTIFACT-REGISTRY.md.
	Assets []Asset `yaml:"assets"`
}

// Asset is one platform-specific file delivered from the R2 artifact registry.
// Integrity is the sha256 (verified at install); the whole bundle tarball is
// itself sha-pinned in the catalogue, so install.json (which carries these
// shas) cannot be tampered with undetected.
type Asset struct {
	Role     string   `yaml:"role" json:"role"`           // "binary" (default, chmod +x) | "data"
	Name     string   `yaml:"name" json:"name"`           // stable id within a platform (default: exec_path basename); referenced by other assets' deps
	OS       string   `yaml:"os" json:"os"`               // linux | darwin
	Arch     string   `yaml:"arch" json:"arch"`           // amd64 | arm64
	URL      string   `yaml:"url" json:"url"`             // https download (R2 public URL)
	SHA256   string   `yaml:"sha256" json:"sha256"`       // 64-hex of the downloaded object; verified after download
	Unpack   string   `yaml:"unpack" json:"unpack"`       // "" (single file) | "tar.gz" (extract archive under $APP)
	ExecPath string   `yaml:"exec_path" json:"exec_path"` // dest under $APP for a single file, or the path INSIDE the extracted tree for an archive (e.g. smolvm-1.2.0-darwin-arm64/smolvm)
	Deps     []string `yaml:"deps" json:"deps"`           // names of assets on the same platform that must install first
	Order    int      `yaml:"order" json:"order"`         // tiebreaker among assets with no dependency relation (ascending)
	Args     []string `yaml:"args" json:"args"`           // optional post-stage invocation, run as "$APP/<exec_path> args..."
}

// AssetName is the stable per-platform id used in dependency edges: the explicit
// name, else the exec_path basename.
func (a Asset) AssetName() string {
	if a.Name != "" {
		return a.Name
	}
	return a.ExecPath[strings.LastIndexByte(a.ExecPath, '/')+1:]
}

// HasAssets reports whether this app delivers native binaries from the registry.
func (c *Config) HasAssets() bool { return len(c.Assets) > 0 }

// PrimaryExecPath is the staged path the fronted command resolves to: the asset
// whose exec_path basename matches command[0] (the binary the adapter execs).
// Empty when there are no assets or no match (the command stays as-is).
func (c *Config) PrimaryExecPath() string {
	if len(c.Backend.Command) == 0 {
		return ""
	}
	cmd := c.Backend.Command[0]
	for _, a := range c.Assets {
		if a.Role == "data" {
			continue
		}
		if base := a.ExecPath[strings.LastIndexByte(a.ExecPath, '/')+1:]; base == cmd || a.ExecPath == cmd {
			return a.ExecPath
		}
	}
	return ""
}

// AssetHosts returns the unique hostnames the adapter must dial to fetch assets,
// for the manifest net.dial grants. Sorted for deterministic generation.
func (c *Config) AssetHosts() []string {
	seen := map[string]bool{}
	var hosts []string
	for _, a := range c.Assets {
		if u, err := url.Parse(a.URL); err == nil && u.Hostname() != "" && !seen[u.Hostname()] {
			seen[u.Hostname()] = true
			hosts = append(hosts, u.Hostname())
		}
	}
	sort.Strings(hosts)
	return hosts
}

// Listing is the store-page metadata that drives the catalogue v2 rich view
// (display_name, vendor, categories, …) and the per-app metadata.json. Optional
// but strongly recommended — without it a published app renders a bare listing.
type Listing struct {
	DisplayName string `yaml:"display_name"` // default: Title-cased namespace
	Tagline     string `yaml:"tagline"`
	// AppDescription is the long-form markdown shown on the store page
	// (`appstore view`). When empty, the one-line Description is used.
	AppDescription string         `yaml:"app_description"`
	Homepage       string         `yaml:"homepage"`
	SourceURL      string         `yaml:"source_url"`
	License        string         `yaml:"license"` // SPDX id, e.g. "MIT", "AGPL-3.0-or-later"
	Categories     []string       `yaml:"categories"`
	Keywords       []string       `yaml:"keywords"`
	Vendor         Vendor         `yaml:"vendor"`
	Changelog      []ChangelogRel `yaml:"changelog"`
}

// Vendor identifies the publisher on the store page.
type Vendor struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Contact string `yaml:"contact"`
}

// ChangelogRel is one release's notes for the store page.
type ChangelogRel struct {
	Version string   `yaml:"version"`
	Date    string   `yaml:"date"`
	Notes   []string `yaml:"notes"`
}

// Publisher carries the path to the ed25519 signing key (generated once via
// `pilotctl appstore gen-key`). The key is gitignored, never committed.
type Publisher struct {
	KeyFile string `yaml:"key_file"`
}

// Backend selects and configures the data plane the adapter forwards to.
type Backend struct {
	Type      string   `yaml:"type"`       // "http" (default) | "cli"
	BaseURL   string   `yaml:"base_url"`   // http: production endpoint, baked in as the default
	Command   []string `yaml:"command"`    // cli: base argv (method args appended)
	EnvPrefix string   `yaml:"env_prefix"` // override env var prefix; default = NAMESPACE upper

	// EnvPassthrough (cli) names host environment variables the fronted CLI is
	// allowed to see, on top of a minimal baseline (PATH, HOME, locale, TMPDIR).
	// The child never inherits the adapter's full environment. e.g.
	//   env_passthrough: [GITHUB_TOKEN, AWS_PROFILE]
	EnvPassthrough []string `yaml:"env_passthrough"`

	// Headers are sent on every backend request (http). Values may contain
	// ${TOKEN} placeholders resolved at runtime from the app's environment or
	// from $APP/secrets.json — so an API key is supplied by the operator at
	// install time and is NEVER baked into the bundle. e.g.
	//   headers: { x-api-key: "${MYAPP_API_KEY}" }
	Headers map[string]string `yaml:"headers"`

	// Auth selects how the adapter authenticates to the backend:
	//   "" / "byo"   — each user supplies their own key (the ${TOKEN} headers above)
	//   "managed"    — Pilot holds ONE master key and meters per user. The generated
	//                  adapter is keyless and points at the Pilot broker (which holds
	//                  the key, identifies the caller, and forwards to base_url). The
	//                  publisher uploads the master key to Pilot once; users bring nothing.
	Auth string `yaml:"auth"`

	// X402 enables transparent, capped payment for paid (x402) APIs: on a
	// backend HTTP 402 the adapter asks the Pilot wallet to satisfy the charge
	// and retries with X-PAYMENT. Presence enables it; it adds an ipc.call
	// grant to the payer app. http backends only.
	X402 *X402 `yaml:"x402"`
}

// BrokerBaseURL is the Pilot managed-key broker the keyless adapter points at.
// The broker maps app id -> {master key, real upstream}, identifies the caller,
// meters per (app, caller), and forwards to the partner API.
const BrokerBaseURL = "https://broker.pilotprotocol.network"

// Managed reports whether this app uses Pilot's managed (shared) master key.
func (c *Config) Managed() bool { return c.Backend.Auth == "managed" }

// AdapterBackendURL is what the generated adapter actually dials: the Pilot
// broker (managed) or the partner API directly (byo). For managed apps the
// partner base_url is registered with the broker, never baked into user hosts.
func (c *Config) AdapterBackendURL() string {
	if c.Managed() {
		return strings.TrimRight(BrokerBaseURL, "/") + "/" + c.ID
	}
	return c.Backend.BaseURL
}

// AdapterBackendHost is the net.dial grant target for the generated adapter:
// the broker host (managed) or the partner host (byo). Managed adapters only
// ever talk to the broker, so that is the only host they may dial.
func (c *Config) AdapterBackendHost() string {
	raw := c.Backend.BaseURL
	if c.Managed() {
		raw = BrokerBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// X402 configures the pay-on-402 flow. All fields have sensible defaults
// (Pilot wallet, Base, USDC); only MaxUSDC is worth setting per app.
type X402 struct {
	Payer    string   `yaml:"payer"`    // payer app id; default io.pilot.wallet
	Method   string   `yaml:"method"`   // payer IPC method; default wallet.evm.satisfy
	Grant    string   `yaml:"grant"`    // ipc.call grant target; default io.pilot.wallet.evm.satisfy
	Networks []string `yaml:"networks"` // allowed networks; default [base]
	Asset    string   `yaml:"asset"`    // required asset symbol; default USDC
	MaxUSDC  string   `yaml:"max_usdc"` // per-call cap in USDC (e.g. "10"); "" = rely on wallet caps
}

// MaxAtomic converts MaxUSDC to USDC atomic units (6 decimals). Empty when no
// cap is set, so generated code can emit a nil cap (wallet caps still apply).
func (x X402) MaxAtomic() string {
	s := strings.TrimSpace(x.MaxUSDC)
	if s == "" {
		return ""
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	for len(frac) < 6 {
		frac += "0"
	}
	frac = frac[:6]
	out := strings.TrimLeft(whole+frac, "0")
	if out == "" {
		out = "0"
	}
	return out
}

// NeedsSecrets reports whether any header value references a ${...} placeholder,
// in which case the manifest must grant fs.read on $APP/secrets.json.
func (b Backend) NeedsSecrets() bool {
	if b.Auth == "managed" {
		return false // the broker holds the key; the adapter carries none
	}
	for _, v := range b.Headers {
		if strings.Contains(v, "${") {
			return true
		}
	}
	return false
}

// Method is one IPC method the adapter exposes, mapped to a backend call.
type Method struct {
	Name      string            `yaml:"name"`      // full IPC name, e.g. weather.current
	Summary   string            `yaml:"summary"`   // one line, shown in <ns>.help
	Kind      string            `yaml:"kind"`      // utility | status | meta (default utility)
	Duration  string            `yaml:"duration"`  // fast | med | slow (default fast)
	Timeout   string            `yaml:"timeout"`   // Go duration; default from duration class
	HTTP      *HTTPRoute        `yaml:"http"`      // http backend route
	CLI       *CLIRoute         `yaml:"cli"`       // cli backend route
	Params    map[string]string `yaml:"params"`    // name -> human description, for help
	Roundtrip string            `yaml:"roundtrip"` // measured warm roundtrip, for help
}

// HTTPRoute maps a method to one backend HTTP endpoint. GET forwards the flat
// JSON payload as a query string; POST forwards it as a JSON body.
type HTTPRoute struct {
	Verb string `yaml:"verb"` // GET (default) | POST
	Path string `yaml:"path"` // e.g. /current
}

// CLIRoute maps a method to a local subprocess invocation. Args may reference
// payload fields as ${field}; ParamsAsFlags appends each payload key as
// --key value. Passthrough instead forwards a verbatim "args" array from the
// call payload — every CLI subcommand is reachable without enumerating it.
type CLIRoute struct {
	Args          []string `yaml:"args"`
	ParamsAsFlags bool     `yaml:"params_as_flags"`
	Passthrough   bool     `yaml:"passthrough"`
}

// Grants tunes the manifest's declared capabilities. The standard set
// (net.dial to the backend host, fs.read $APP/config.json, audit.log) is
// generated automatically; this only tunes the rate limit and adds extras.
type Grants struct {
	RatePerMin int        `yaml:"rate_per_min"` // net.dial rate; default 120
	Extra      []RawGrant `yaml:"extra"`        // verbatim extra grants
}

// RawGrant is a grant passed through to the manifest unchanged.
type RawGrant struct {
	Cap    string `yaml:"cap" json:"cap"`
	Target string `yaml:"target" json:"target"`
}

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9_-]*[a-z0-9])?)+$`)
	semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// knownOS / knownArch are the host targets the registry + staging understand.
// These match scaffold/build platform tuples (DefaultPlatforms) and the daemon's
// runtime.GOOS/GOARCH values.
var (
	knownOS   = map[string]bool{"linux": true, "darwin": true}
	knownArch = map[string]bool{"amd64": true, "arm64": true}
)

// Parse decodes a pilot.app.yaml document (strict: unknown keys are errors, so
// typos surface instead of being silently ignored).
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse pilot.app.yaml: %w", err)
	}
	return &c, nil
}

// Resolve fills derived defaults in place. Call before Validate/Generate.
func (c *Config) Resolve() {
	if c.ManifestVersion == 0 {
		c.ManifestVersion = 1
	}
	if c.Backend.Type == "" {
		c.Backend.Type = "http"
	}
	if c.Namespace == "" {
		if i := strings.LastIndexByte(c.ID, '.'); i >= 0 {
			c.Namespace = c.ID[i+1:]
		} else {
			c.Namespace = c.ID
		}
	}
	if c.BinaryName == "" {
		c.BinaryName = c.Namespace + "-app"
	}
	if c.GoModule == "" {
		c.GoModule = "github.com/pilot-protocol/" + c.BinaryName
	}
	if c.AppStoreModule == "" {
		c.AppStoreModule = defaultAppStoreModule
	}
	if c.Backend.EnvPrefix == "" {
		c.Backend.EnvPrefix = strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(c.Namespace))
	}
	if c.Publisher.KeyFile == "" {
		c.Publisher.KeyFile = c.Namespace + "-publisher.key"
	}
	if c.Listing.DisplayName == "" && c.Namespace != "" {
		c.Listing.DisplayName = strings.ToUpper(c.Namespace[:1]) + c.Namespace[1:]
	}
	if c.Grants.RatePerMin == 0 {
		c.Grants.RatePerMin = 120
	}
	if x := c.Backend.X402; x != nil {
		if x.Payer == "" {
			x.Payer = "io.pilot.wallet"
		}
		if x.Method == "" {
			x.Method = "wallet.evm.satisfy"
		}
		if x.Grant == "" {
			x.Grant = "io.pilot.wallet.evm.satisfy"
		}
		if len(x.Networks) == 0 {
			x.Networks = []string{"base"}
		}
		if x.Asset == "" {
			x.Asset = "USDC"
		}
	}
	for i := range c.Methods {
		m := &c.Methods[i]
		if m.Kind == "" {
			m.Kind = "utility"
		}
		if m.Duration == "" {
			m.Duration = "fast"
		}
		if m.HTTP != nil && m.HTTP.Verb == "" {
			m.HTTP.Verb = "GET"
		}
		if m.HTTP != nil {
			m.HTTP.Verb = strings.ToUpper(m.HTTP.Verb)
		}
	}
}

// Validate returns all spec errors found, so they can be fixed in one pass.
func (c *Config) Validate() []error {
	var errs []error
	if !idPattern.MatchString(c.ID) {
		errs = append(errs, fmt.Errorf("id %q must be reverse-DNS-shaped (e.g. io.pilot.weather)", c.ID))
	}
	if !semverPattern.MatchString(c.AppVersion) {
		errs = append(errs, fmt.Errorf("app_version %q must be semver (e.g. 0.1.0)", c.AppVersion))
	}
	if strings.TrimSpace(c.Description) == "" {
		errs = append(errs, fmt.Errorf("description must not be empty"))
	}
	switch c.Backend.Type {
	case "http":
		if c.Backend.BaseURL == "" {
			errs = append(errs, fmt.Errorf("backend.base_url is required for an http backend"))
		} else if u, err := url.Parse(c.Backend.BaseURL); err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("backend.base_url %q is not a valid absolute URL", c.Backend.BaseURL))
		}
	case "cli":
		if len(c.Backend.Command) == 0 {
			errs = append(errs, fmt.Errorf("backend.command is required for a cli backend"))
		}
	default:
		errs = append(errs, fmt.Errorf("backend.type %q must be \"http\" or \"cli\"", c.Backend.Type))
	}
	if x := c.Backend.X402; x != nil {
		if c.Backend.Type != "http" {
			errs = append(errs, fmt.Errorf("backend.x402 is only valid for an http backend"))
		}
		if strings.TrimSpace(x.Grant) == "" {
			errs = append(errs, fmt.Errorf("backend.x402.grant (ipc.call target) must not be empty"))
		}
		if x.MaxUSDC != "" {
			if m := x.MaxAtomic(); m == "" || m == "0" {
				errs = append(errs, fmt.Errorf("backend.x402.max_usdc %q is not a positive amount", x.MaxUSDC))
			}
		}
	}
	errs = append(errs, c.validateAssets()...)
	if len(c.Methods) == 0 {
		errs = append(errs, fmt.Errorf("at least one method must be declared"))
	}
	seen := map[string]bool{}
	for i, m := range c.Methods {
		if strings.TrimSpace(m.Name) == "" {
			errs = append(errs, fmt.Errorf("methods[%d].name must not be empty", i))
			continue
		}
		if !strings.HasPrefix(m.Name, c.Namespace+".") {
			errs = append(errs, fmt.Errorf("methods[%d].name %q must be prefixed %q", i, m.Name, c.Namespace+"."))
		}
		if m.Name == c.Namespace+".help" {
			errs = append(errs, fmt.Errorf("methods[%d]: %q is reserved — the generator adds it automatically", i, m.Name))
		}
		if seen[m.Name] {
			errs = append(errs, fmt.Errorf("methods[%d]: duplicate method %q", i, m.Name))
		}
		seen[m.Name] = true
		switch m.Kind {
		case "utility", "status", "meta":
		default:
			errs = append(errs, fmt.Errorf("methods[%d].kind %q must be utility|status|meta", i, m.Kind))
		}
		switch m.Duration {
		case "fast", "med", "slow":
		default:
			errs = append(errs, fmt.Errorf("methods[%d].duration %q must be fast|med|slow", i, m.Duration))
		}
		if m.Timeout != "" {
			if _, err := time.ParseDuration(m.Timeout); err != nil {
				errs = append(errs, fmt.Errorf("methods[%d].timeout %q is not a Go duration: %w", i, m.Timeout, err))
			}
		}
		switch c.Backend.Type {
		case "http":
			if m.HTTP == nil {
				errs = append(errs, fmt.Errorf("methods[%d] (%s): http backend requires an http: route", i, m.Name))
			} else {
				if m.HTTP.Path == "" || !strings.HasPrefix(m.HTTP.Path, "/") {
					errs = append(errs, fmt.Errorf("methods[%d].http.path must start with /", i))
				}
				if m.HTTP.Verb != "GET" && m.HTTP.Verb != "POST" {
					errs = append(errs, fmt.Errorf("methods[%d].http.verb %q must be GET or POST", i, m.HTTP.Verb))
				}
			}
		case "cli":
			if m.CLI == nil {
				errs = append(errs, fmt.Errorf("methods[%d] (%s): cli backend requires a cli: route", i, m.Name))
			} else if m.CLI.Passthrough {
				if len(m.CLI.Args) > 0 {
					errs = append(errs, fmt.Errorf("methods[%d] (%s): cli.passthrough takes argv from the call payload — remove cli.args", i, m.Name))
				}
				if m.CLI.ParamsAsFlags {
					errs = append(errs, fmt.Errorf("methods[%d] (%s): cli.params_as_flags has no effect with passthrough", i, m.Name))
				}
			} else if len(m.CLI.Args) == 0 && !m.CLI.ParamsAsFlags {
				errs = append(errs, fmt.Errorf("methods[%d] (%s): cli route needs args, params_as_flags, or passthrough", i, m.Name))
			}
		}
	}
	return errs
}

// validateAssets enforces the registry-delivery rules: assets are cli-only,
// each names a known os/arch, an https URL, a 64-hex sha256, and an exec_path
// that stays under $APP (no absolute path, no "..", no leading slash). Orders
// must be unique so the install sequence is deterministic, and (os,arch,role)
// must be unique so the host match is unambiguous.
func (c *Config) validateAssets() []error {
	if len(c.Assets) == 0 {
		return nil
	}
	var errs []error
	if c.Backend.Type != "cli" {
		errs = append(errs, fmt.Errorf("assets are only valid for a cli backend (an http app delivers no binary)"))
	}
	// Orders and binary roles are scoped per host platform: each host installs
	// only its own (os,arch) assets, so two platforms may both use order 1, but
	// within one platform the order must be unique (deterministic sequence) and a
	// platform must not ship two binaries for the same exec_path.
	orders := map[string]bool{}
	platforms := map[string]bool{}
	for i, a := range c.Assets {
		role := a.Role
		if role == "" {
			role = "binary"
		}
		if role != "binary" && role != "data" {
			errs = append(errs, fmt.Errorf("assets[%d].role %q must be \"binary\" or \"data\"", i, a.Role))
		}
		if a.Unpack != "" && a.Unpack != "tar.gz" {
			errs = append(errs, fmt.Errorf("assets[%d].unpack %q must be \"\" or \"tar.gz\"", i, a.Unpack))
		}
		if !knownOS[a.OS] {
			errs = append(errs, fmt.Errorf("assets[%d].os %q must be linux or darwin", i, a.OS))
		}
		if !knownArch[a.Arch] {
			errs = append(errs, fmt.Errorf("assets[%d].arch %q must be amd64 or arm64", i, a.Arch))
		}
		if u, err := url.Parse(a.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("assets[%d].url %q must be an absolute https URL", i, a.URL))
		}
		if !sha256Pattern.MatchString(a.SHA256) {
			errs = append(errs, fmt.Errorf("assets[%d].sha256 %q must be 64 lowercase hex chars", i, a.SHA256))
		}
		if a.ExecPath == "" || strings.HasPrefix(a.ExecPath, "/") || strings.Contains(a.ExecPath, "..") {
			errs = append(errs, fmt.Errorf("assets[%d].exec_path %q must be a relative path under $APP (no leading / and no \"..\")", i, a.ExecPath))
		}
		plat := a.OS + "/" + a.Arch
		orderKey := fmt.Sprintf("%s#%d", plat, a.Order)
		if orders[orderKey] {
			errs = append(errs, fmt.Errorf("assets[%d]: duplicate install order %d for %s — orders must be unique within a platform", i, a.Order, plat))
		}
		orders[orderKey] = true
		key := plat + "/" + a.ExecPath
		if platforms[key] {
			errs = append(errs, fmt.Errorf("assets[%d]: duplicate asset for %s at %s", i, plat, a.ExecPath))
		}
		platforms[key] = true
	}
	// Per-platform: dependency names must resolve to a sibling and form a DAG.
	for _, plat := range c.assetPlatforms() {
		if _, err := c.ResolveAssets(plat[0], plat[1]); err != nil {
			errs = append(errs, fmt.Errorf("assets for %s/%s: %w", plat[0], plat[1], err))
		}
	}
	return errs
}

// assetPlatforms lists the distinct (os,arch) tuples present in Assets.
func (c *Config) assetPlatforms() [][2]string {
	seen := map[string]bool{}
	var out [][2]string
	for _, a := range c.Assets {
		k := a.OS + "/" + a.Arch
		if !seen[k] {
			seen[k] = true
			out = append(out, [2]string{a.OS, a.Arch})
		}
	}
	return out
}

// ResolveAssets returns the assets for one host platform in install order: a
// topological sort over `deps` (an asset installs after everything it depends
// on), with `order` then name as the deterministic tiebreaker among assets that
// have no dependency relation. Errors on an unknown dep name or a cycle.
func (c *Config) ResolveAssets(os, arch string) ([]Asset, error) {
	var plat []Asset
	for _, a := range c.Assets {
		if a.OS == os && a.Arch == arch {
			plat = append(plat, a)
		}
	}
	byName := map[string]Asset{}
	for _, a := range plat {
		byName[a.AssetName()] = a
	}
	// Kahn's algorithm with a deterministic ready-set ordering.
	indeg := map[string]int{}
	for _, a := range plat {
		indeg[a.AssetName()] = 0
	}
	for _, a := range plat {
		for _, d := range a.Deps {
			if _, ok := byName[d]; !ok {
				return nil, fmt.Errorf("asset %q depends on unknown asset %q", a.AssetName(), d)
			}
			indeg[a.AssetName()]++
		}
	}
	less := func(x, y Asset) bool {
		if x.Order != y.Order {
			return x.Order < y.Order
		}
		return x.AssetName() < y.AssetName()
	}
	var out []Asset
	for len(out) < len(plat) {
		var ready []Asset
		for _, a := range plat {
			if indeg[a.AssetName()] == 0 {
				ready = append(ready, a)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("dependency cycle among assets")
		}
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		next := ready[0]
		indeg[next.AssetName()] = -1 // mark consumed
		out = append(out, next)
		for _, a := range plat {
			for _, d := range a.Deps {
				if d == next.AssetName() {
					indeg[a.AssetName()]--
				}
			}
		}
	}
	return out, nil
}

// BackendHost returns the net.dial target for the grant block (http only).
func (c *Config) BackendHost() string {
	u, err := url.Parse(c.Backend.BaseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// SortedHeaderKeys gives deterministic header ordering for generated code.
func (b Backend) SortedHeaderKeys() []string {
	keys := make([]string, 0, len(b.Headers))
	for k := range b.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TimeoutFor returns the per-call deadline for a method: explicit value if set,
// else by duration class (slow methods get the long deadline).
func (m Method) TimeoutFor() string {
	if m.Timeout != "" {
		return m.Timeout
	}
	if m.Duration == "slow" {
		return "180s"
	}
	return "60s"
}

// SortedParamKeys gives deterministic param ordering for generated code.
func (m Method) SortedParamKeys() []string {
	keys := make([]string, 0, len(m.Params))
	for k := range m.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
