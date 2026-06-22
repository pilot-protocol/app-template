package publish

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/pilot-protocol/app-template/internal/scaffold"
)

// Submission is the full, structured publish request the website form collects
// and the API receives as JSON. It is richer than scaffold.Config: it also
// carries review-only metadata (vendor free-text, agent-usage, capabilities,
// binary-delivery info) that drives the admin case report but isn't part of the
// buildable adapter. ToConfig derives the buildable spec.
type Submission struct {
	// App identity + what it does.
	ID          string `json:"id"`          // io.pilot.<name> (prefix mandatory)
	Version     string `json:"version"`     // semver
	Description string `json:"description"` // one-line: what the app does

	Email string `json:"email"` // publisher email (for submission/decision notifications)

	Backend SubBackend  `json:"backend"`
	Methods []SubMethod `json:"methods"`
	Listing SubListing  `json:"listing"`
	Vendor  SubVendor   `json:"vendor"`
}

// SubBackend selects and configures the data plane the adapter forwards to:
// either an HTTP API (Type "http", the default) or a local CLI (Type "cli").
type SubBackend struct {
	// Type is "http" (default) or "cli". Empty means http for back-compat with
	// older form payloads that predate the selector.
	Type string `json:"type"`

	// --- http fields ---
	BaseURL string      `json:"base_url"`
	Headers []SubHeader `json:"headers"` // auth/extra headers; values may use ${TOKEN}
	// Auth selects how the adapter authenticates to the backend:
	//   "" / "byo"  — each user brings their own key (the ${TOKEN} headers)
	//   "managed"   — Pilot holds one master key and meters per user; the
	//                 generated adapter is keyless and points at the broker.
	Auth string `json:"auth"`
	// Quota is the per-caller call cap the broker enforces for a managed app
	// (0 = unlimited). Set at publish time so the rate limit ships with the app.
	Quota int `json:"quota"`

	// --- cli fields ---
	// Command is the base argv the adapter execs (e.g. ["gh"] or ["python","-m","tool"]).
	Command []string `json:"command"`
	// EnvPassthrough names host env vars the fronted CLI may see, on top of a
	// minimal baseline (PATH/HOME/locale/TMPDIR). The child never inherits the
	// adapter's full environment.
	EnvPassthrough []string `json:"env_passthrough"`
}

// IsCLI reports whether this submission fronts a local CLI rather than an HTTP API.
func (b SubBackend) IsCLI() bool { return b.Type == "cli" }

// Managed reports whether this submission uses Pilot's managed master key. Only
// meaningful for http backends (a cli app holds no key).
func (b SubBackend) Managed() bool { return !b.IsCLI() && b.Auth == "managed" }

// SubHeader is one request header. Value may contain ${TOKEN} placeholders that
// the operator supplies at install (env or $APP/secrets.json) — never baked in.
type SubHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SubMethod is one IPC method the agent can call, mapped to a backend route.
// Exactly one of HTTP / CLI is meaningful, selected by the backend type.
type SubMethod struct {
	Name        string      `json:"name"`        // <ns>.<verb>, e.g. weather.current
	Description string      `json:"description"` // full description, shown in help
	Latency     string      `json:"latency"`     // fast | med | slow (REQUIRED)
	Timeout     string      `json:"timeout"`     // optional Go duration (e.g. "280s") overriding the latency-class default
	HTTP        SubRoute    `json:"http"`        // http backend route
	CLI         SubCLIRoute `json:"cli"`         // cli backend route
	Params      []SubParam  `json:"params"`
}

// SubRoute is the backend HTTP mapping for a method.
type SubRoute struct {
	Verb string `json:"verb"` // GET | POST
	Path string `json:"path"` // e.g. /current
}

// SubCLIRoute is the backend CLI mapping for a method. Enumerated methods bake
// Args (with ${field} placeholders from the payload) and optionally append each
// payload field as --key value (ParamsAsFlags). Passthrough instead forwards a
// verbatim "args" array, so every subcommand of the fronted CLI is reachable —
// the "translate all CLI commands" shape.
type SubCLIRoute struct {
	Args          []string `json:"args"`
	ParamsAsFlags bool     `json:"params_as_flags"`
	Passthrough   bool     `json:"passthrough"`
}

// SubParam is one structured input parameter (vs the old free-text field).
type SubParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string | int | bool | number
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// SubListing is everything needed to display the app in the store, plus the
// optional native-binary delivery info.
type SubListing struct {
	DisplayName    string   `json:"display_name"`
	Tagline        string   `json:"tagline"`
	AppDescription string   `json:"app_description"` // long-form, markdown
	License        string   `json:"license"`
	Homepage       string   `json:"homepage"`
	SourceURL      string   `json:"source_url"`
	Categories     []string `json:"categories"`
	Keywords       []string `json:"keywords"`
	RequiresBinary bool     `json:"requires_binary"` // native app that ships a real binary
	BinaryURL      string   `json:"binary_url"`      // where that binary is hosted (per NATIVE-APPS.md)
}

// SubVendor is everything about the publisher, plus the two agent-facing
// free-text sections that the reviewer reads.
type SubVendor struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	Contact      string `json:"contact"`
	AgentUsage   string `json:"agent_usage"`  // "How will autonomous AI agents use this app?"
	Capabilities string `json:"capabilities"` // "List of all capabilities"
}

var (
	subID     = regexp.MustCompile(`^io\.pilot\.[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	subSemver = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	subLat    = map[string]bool{"fast": true, "med": true, "slow": true}
)

// LatencyRef is the agent-facing reference for the three classes (shown in UI + help).
var LatencyRef = map[string]string{
	"fast": "under 5 seconds",
	"med":  "up to 15 seconds",
	"slow": "up to 1 minute",
}

// Namespace is the method prefix, derived from the id (io.pilot.<ns>).
func (s Submission) Namespace() string {
	if i := strings.LastIndexByte(s.ID, '.'); i >= 0 {
		return s.ID[i+1:]
	}
	return s.ID
}

// Validate returns all submission errors (server-authoritative).
func (s Submission) Validate() []string {
	var e []string
	if !subID.MatchString(s.ID) {
		e = append(e, "App ID must be io.pilot.<name> (lowercase, e.g. io.pilot.weather)")
	}
	if !subSemver.MatchString(s.Version) {
		e = append(e, "Version must be semver, e.g. 0.1.0")
	}
	if strings.TrimSpace(s.Description) == "" {
		e = append(e, "App description is required")
	}
	if !reEmail.MatchString(strings.TrimSpace(s.Email)) {
		e = append(e, "A valid email is required")
	}
	if s.Backend.IsCLI() {
		if len(s.Backend.Command) == 0 || strings.TrimSpace(s.Backend.Command[0]) == "" {
			e = append(e, `CLI backend requires a command (the base argv, e.g. ["gh"])`)
		}
	} else if !reURL.MatchString(strings.TrimSpace(s.Backend.BaseURL)) {
		e = append(e, "Backend base URL must be an absolute http(s) URL")
	}
	if len(s.Methods) == 0 {
		e = append(e, "Add at least one method")
	}
	ns := s.Namespace()
	seen := map[string]bool{}
	for i, m := range s.Methods {
		n := strings.TrimSpace(m.Name)
		if n == "" {
			e = append(e, fmt.Sprintf("Method %d: name is required", i+1))
			continue
		}
		if !strings.HasPrefix(n, ns+".") {
			e = append(e, fmt.Sprintf("Method %q must be prefixed %q.", n, ns))
		}
		if seen[n] {
			e = append(e, fmt.Sprintf("Duplicate method %q", n))
		}
		seen[n] = true
		if !subLat[m.Latency] {
			e = append(e, fmt.Sprintf("Method %q: latency is required (fast/med/slow)", n))
		}
		if strings.TrimSpace(m.Description) == "" {
			e = append(e, fmt.Sprintf("Method %q: description is required", n))
		}
		if s.Backend.IsCLI() {
			switch {
			case m.CLI.Passthrough:
				if len(m.CLI.Args) > 0 || m.CLI.ParamsAsFlags {
					e = append(e, fmt.Sprintf("Method %q: passthrough takes argv from the call — remove args/params_as_flags", n))
				}
			case len(m.CLI.Args) == 0 && !m.CLI.ParamsAsFlags:
				e = append(e, fmt.Sprintf("Method %q: CLI method needs args, params_as_flags, or passthrough", n))
			}
		} else if m.HTTP.Path == "" || !strings.HasPrefix(m.HTTP.Path, "/") {
			e = append(e, fmt.Sprintf("Method %q: path must start with /", n))
		}
	}
	return e
}

// ToConfig derives the buildable adapter spec from the submission (the fields
// the generator needs). Review-only fields (vendor free-text, agent-usage,
// capabilities, binary URL) are intentionally not part of it.
func (s Submission) ToConfig() *scaffold.Config {
	backend := scaffold.Backend{Type: "http", BaseURL: s.Backend.BaseURL, Auth: s.Backend.Auth}
	if s.Backend.IsCLI() {
		backend = scaffold.Backend{Type: "cli", Command: s.Backend.Command, EnvPassthrough: s.Backend.EnvPassthrough}
	}
	cfg := &scaffold.Config{
		ID:          s.ID,
		AppVersion:  s.Version,
		Description: s.Description,
		Backend:     backend,
		Listing: scaffold.Listing{
			DisplayName: s.Listing.DisplayName,
			Tagline:     s.Listing.Tagline,
			Homepage:    s.Listing.Homepage,
			SourceURL:   s.Listing.SourceURL,
			License:     s.Listing.License,
			Categories:  s.Listing.Categories,
			Keywords:    s.Listing.Keywords,
			Vendor:      scaffold.Vendor{Name: s.Vendor.Name, URL: s.Vendor.URL, Contact: s.Vendor.Contact},
		},
	}
	// HTTP byo apps carry auth headers; managed apps are keyless (the broker
	// holds the key) and cli apps have no HTTP headers at all.
	if !s.Backend.IsCLI() && !s.Backend.Managed() {
		headers := map[string]string{}
		for _, h := range s.Backend.Headers {
			if strings.TrimSpace(h.Name) != "" {
				headers[h.Name] = h.Value
			}
		}
		if len(headers) > 0 {
			cfg.Backend.Headers = headers
		}
	}
	for _, m := range s.Methods {
		params := map[string]string{}
		for _, p := range m.Params {
			if p.Name == "" {
				continue
			}
			desc := p.Type
			if p.Required {
				desc += " (required)"
			}
			if p.Description != "" {
				desc += " — " + p.Description
			}
			params[p.Name] = desc
		}
		method := scaffold.Method{
			Name:     m.Name,
			Summary:  m.Description, // help "summary" carries the description
			Duration: m.Latency,
			Timeout:  m.Timeout, // explicit per-method timeout (overrides the latency-class default)
			Params:   params,
		}
		if s.Backend.IsCLI() {
			method.CLI = &scaffold.CLIRoute{
				Args:          m.CLI.Args,
				ParamsAsFlags: m.CLI.ParamsAsFlags,
				Passthrough:   m.CLI.Passthrough,
			}
		} else {
			method.HTTP = &scaffold.HTTPRoute{Verb: orDefault(m.HTTP.Verb, "GET"), Path: m.HTTP.Path}
		}
		cfg.Methods = append(cfg.Methods, method)
	}
	cfg.Resolve()
	return cfg
}

var reURL = regexp.MustCompile(`^https?://[^\s/]+`)
var reEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// HelpPreview returns the live <ns>.help document and the pilotctl command lines
// for the current methods — server-generated so the website preview matches what
// ships. Safe on partial/invalid submissions (skips unnamed methods).
func (s Submission) HelpPreview() (HelpDoc, []string) {
	ns := s.Namespace()
	doc := HelpDoc{App: s.ID, Description: s.Description, DurationClasses: LatencyRef}
	var cmds []string
	add := func(m SubMethod) {
		pm := map[string]string{}
		for _, p := range m.Params {
			if p.Name != "" {
				pm[p.Name] = p.Type
			}
		}
		doc.Methods = append(doc.Methods, HelpMethod{Method: m.Name, Summary: m.Description, Duration: orDefault(m.Latency, "fast"), Params: pm})
		// pilotctl call line. A cli passthrough method takes a verbatim argv
		// array, so its payload is {"args":[...]}; everything else shows a JSON
		// skeleton of the named params.
		var payload string
		if s.Backend.IsCLI() && m.CLI.Passthrough {
			payload = `{"args":["<subcommand>","<arg>"]}`
		} else {
			var kv []string
			ks := make([]string, 0, len(pm))
			for k := range pm {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			for _, k := range ks {
				kv = append(kv, fmt.Sprintf("%q:%q", k, "<"+pm[k]+">"))
			}
			payload = "{" + strings.Join(kv, ",") + "}"
		}
		cmds = append(cmds, fmt.Sprintf("pilotctl appstore call %s %s '%s'", s.ID, m.Name, payload))
	}
	for _, m := range s.Methods {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		add(m)
	}
	// The always-present discovery method.
	doc.Methods = append(doc.Methods, HelpMethod{Method: ns + ".help", Summary: "Discovery: every method with params, latency, and description.", Duration: "fast"})
	cmds = append([]string{
		"pilotctl appstore install " + s.ID,
		fmt.Sprintf("pilotctl appstore call %s %s.help '{}'", s.ID, ns),
	}, cmds...)
	return doc, cmds
}

// HelpDoc / HelpMethod mirror the <ns>.help shape for the live preview.
type HelpDoc struct {
	App             string            `json:"app"`
	Description     string            `json:"description"`
	DurationClasses map[string]string `json:"duration_classes"`
	Methods         []HelpMethod      `json:"methods"`
}

type HelpMethod struct {
	Method   string            `json:"method"`
	Summary  string            `json:"summary"`
	Duration string            `json:"duration"`
	Params   map[string]string `json:"params,omitempty"`
}
