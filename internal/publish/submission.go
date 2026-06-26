package publish

import (
	"fmt"
	"net/url"
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

	// Artifacts is the native-binary delivery set for a cli app: the
	// platform-specific binaries the publisher uploaded to the Pilot R2 artifact
	// registry in the form's Artifacts step, with the install order and any
	// optional install args. Empty for http apps and for cli apps whose command
	// is already present on the host. ToConfig maps these to scaffold.Asset.
	Artifacts []SubArtifact `json:"artifacts"`
}

// SubArtifact is one uploaded, platform-specific, signed binary in the publish
// form's Artifacts step. URL is the R2 location returned by the presign upload;
// SHA256 is verified server-side against the stored object before the case is
// accepted, and again on the host at install. Mirrors scaffold.Asset.
type SubArtifact struct {
	Role     string   `json:"role"`      // "binary" (default) | "data"
	Name     string   `json:"name"`      // per-platform id (default: exec_path basename); referenced by deps
	OS       string   `json:"os"`        // linux | darwin
	Arch     string   `json:"arch"`      // amd64 | arm64
	URL      string   `json:"url"`       // R2 public URL
	SHA256   string   `json:"sha256"`    // 64-hex of the uploaded object
	Unpack   string   `json:"unpack"`    // "" (single file) | "tar.gz" (extract under $APP)
	ExecPath string   `json:"exec_path"` // dest under $APP, or path inside the extracted tree
	Deps     []string `json:"deps"`      // names of same-platform artifacts installed first
	Order    int      `json:"order"`     // tiebreaker among independent artifacts (per platform)
	Args     []string `json:"args"`      // optional post-stage install args
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
	// In is the param's HTTP request location (http backends only). One of
	// query | path | path_raw | body | header; empty keeps the verb/path default
	// (a {name} placeholder → path, otherwise GET→query, POST/PUT/PATCH→body).
	// path_raw fills a {name} placeholder WITHOUT URL-escaping, for URL-in-path
	// APIs (e.g. GET base/<rawurl>) where escaping the scheme breaks the backend.
	In string `json:"in"`
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
	e = append(e, s.validateArtifacts()...)
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
		} else {
			e = append(e, validateParamLocations(n, m)...)
		}
	}
	return e
}

// subParamIn is the closed set of param request locations (empty = default).
var subParamIn = map[string]bool{
	"query": true, "path": true, "path_raw": true, "body": true, "header": true,
}

// validateParamLocations checks the per-param `in` rules for an http method, so
// a publisher gets clear, server-authoritative errors before any build: `in`
// must be one of the five values, and a path/path_raw param MUST correspond to a
// {name} placeholder in the route path (otherwise it has nowhere to go).
func validateParamLocations(method string, m SubMethod) []string {
	var e []string
	placeholder := map[string]bool{}
	for _, p := range scaffold.PathPlaceholders(m.HTTP.Path) {
		placeholder[p] = true
	}
	for _, p := range m.Params {
		name := strings.TrimSpace(p.Name)
		if name == "" || p.In == "" {
			continue
		}
		if !subParamIn[p.In] {
			e = append(e, fmt.Sprintf("Method %q, param %q: in %q must be one of query, path, path_raw, body, header", method, name, p.In))
			continue
		}
		if (p.In == "path" || p.In == "path_raw") && !placeholder[name] {
			e = append(e, fmt.Sprintf("Method %q, param %q: in %q needs a matching {%s} placeholder in path %q", method, name, p.In, name, m.HTTP.Path))
		}
		if p.In == "header" && name == "" {
			e = append(e, fmt.Sprintf("Method %q: a header param needs a non-empty name", method))
		}
	}
	return e
}

var (
	subSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	subOSOK   = map[string]bool{"linux": true, "darwin": true}
	subArchOK = map[string]bool{"amd64": true, "arm64": true}
)

// validateArtifacts mirrors the scaffold asset rules at the submission boundary
// so a publisher gets clear, server-authoritative errors before any build:
// artifacts are cli-only, each names a known os/arch, an https R2 URL, a 64-hex
// sha256, and a relative exec_path under $APP; install order is unique per
// platform. (The sha is additionally re-verified against the stored R2 object on
// submit, and on the host at install.)
func (s Submission) validateArtifacts() []string {
	if len(s.Artifacts) == 0 {
		return nil
	}
	var e []string
	if !s.Backend.IsCLI() {
		e = append(e, "Artifacts (binary delivery) are only valid for a cli backend")
	}
	orders := map[string]bool{}
	for i, a := range s.Artifacts {
		role := a.Role
		if role == "" {
			role = "binary"
		}
		if role != "binary" && role != "data" {
			e = append(e, fmt.Sprintf("Artifact %d: role %q must be binary or data", i+1, a.Role))
		}
		if a.Unpack != "" && a.Unpack != "tar.gz" {
			e = append(e, fmt.Sprintf("Artifact %d: unpack %q must be empty or \"tar.gz\"", i+1, a.Unpack))
		}
		if !subOSOK[a.OS] {
			e = append(e, fmt.Sprintf("Artifact %d: os %q must be linux or darwin", i+1, a.OS))
		}
		if !subArchOK[a.Arch] {
			e = append(e, fmt.Sprintf("Artifact %d: arch %q must be amd64 or arm64", i+1, a.Arch))
		}
		if u, err := url.Parse(strings.TrimSpace(a.URL)); err != nil || u.Scheme != "https" || u.Host == "" {
			e = append(e, fmt.Sprintf("Artifact %d: url must be an absolute https URL (the R2 upload location)", i+1))
		}
		if !subSHA256.MatchString(a.SHA256) {
			e = append(e, fmt.Sprintf("Artifact %d: sha256 must be 64 lowercase hex chars", i+1))
		}
		if a.ExecPath == "" || strings.HasPrefix(a.ExecPath, "/") || strings.Contains(a.ExecPath, "..") {
			e = append(e, fmt.Sprintf("Artifact %d: exec_path must be a relative path under $APP (no leading / or \"..\")", i+1))
		}
		key := fmt.Sprintf("%s/%s#%d", a.OS, a.Arch, a.Order)
		if orders[key] {
			e = append(e, fmt.Sprintf("Artifact %d: duplicate install order %d for %s/%s", i+1, a.Order, a.OS, a.Arch))
		}
		orders[key] = true
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
			DisplayName:    s.Listing.DisplayName,
			Tagline:        s.Listing.Tagline,
			AppDescription: s.Listing.AppDescription,
			Homepage:       s.Listing.Homepage,
			SourceURL:      s.Listing.SourceURL,
			License:        s.Listing.License,
			Categories:     s.Listing.Categories,
			Keywords:       s.Listing.Keywords,
			Vendor:         scaffold.Vendor{Name: s.Vendor.Name, URL: s.Vendor.URL, Contact: s.Vendor.Contact},
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
			route := &scaffold.HTTPRoute{Verb: orDefault(m.HTTP.Verb, "GET"), Path: m.HTTP.Path}
			// Carry each param's explicit request location so the generator can
			// resolve query/path/path_raw/body/header placement. Omitted `in`
			// keeps the verb/path default (back-compat).
			for _, p := range m.Params {
				if p.Name == "" || p.In == "" {
					continue
				}
				if route.ParamIn == nil {
					route.ParamIn = map[string]string{}
				}
				route.ParamIn[p.Name] = p.In
			}
			method.HTTP = route
		}
		cfg.Methods = append(cfg.Methods, method)
	}
	for _, a := range s.Artifacts {
		cfg.Assets = append(cfg.Assets, scaffold.Asset{
			Role: a.Role, Name: a.Name, OS: a.OS, Arch: a.Arch, URL: a.URL, SHA256: a.SHA256,
			Unpack: a.Unpack, ExecPath: a.ExecPath, Deps: a.Deps, Order: a.Order, Args: a.Args,
		})
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
