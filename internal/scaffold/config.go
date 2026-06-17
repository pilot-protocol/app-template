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
const defaultAppStoreModule = "github.com/pilot-protocol/app-store v1.0.1-beta.1.0.20260609061942-8852c785a264"

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
}

// Listing is the store-page metadata that drives the catalogue v2 rich view
// (display_name, vendor, categories, …) and the per-app metadata.json. Optional
// but strongly recommended — without it a published app renders a bare listing.
type Listing struct {
	DisplayName string         `yaml:"display_name"` // default: Title-cased namespace
	Tagline     string         `yaml:"tagline"`
	Homepage    string         `yaml:"homepage"`
	SourceURL   string         `yaml:"source_url"`
	License     string         `yaml:"license"` // SPDX id, e.g. "MIT", "AGPL-3.0-or-later"
	Categories  []string       `yaml:"categories"`
	Keywords    []string       `yaml:"keywords"`
	Vendor      Vendor         `yaml:"vendor"`
	Changelog   []ChangelogRel `yaml:"changelog"`
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

	// Headers are sent on every backend request (http). Values may contain
	// ${TOKEN} placeholders resolved at runtime from the app's environment or
	// from $APP/secrets.json — so an API key is supplied by the operator at
	// install time and is NEVER baked into the bundle. e.g.
	//   headers: { x-api-key: "${MYAPP_API_KEY}" }
	Headers map[string]string `yaml:"headers"`
}

// NeedsSecrets reports whether any header value references a ${...} placeholder,
// in which case the manifest must grant fs.read on $APP/secrets.json.
func (b Backend) NeedsSecrets() bool {
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

// CLIRoute maps a method to a local subprocess invocation (planned archetype).
// Args may reference payload fields as {{.field}}; ParamsAsFlags appends each
// payload key as --key value.
type CLIRoute struct {
	Args          []string `yaml:"args"`
	ParamsAsFlags bool     `yaml:"params_as_flags"`
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
			errs = append(errs, fmt.Errorf("methods[%d].name %q must be prefixed %q.", i, m.Name, c.Namespace))
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
			}
		}
	}
	return errs
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
