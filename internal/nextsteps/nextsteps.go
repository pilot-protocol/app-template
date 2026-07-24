// Package nextsteps defines the "next steps graph" — the dynamic context an
// agent is shown after EVERY `pilotctl appstore call`, on both success and
// failure, telling it what to run next.
//
// The problem it solves: an autonomous agent installs an app and never uses it,
// because nothing in the call's output tells it where the flow goes. A product
// demo (see internal/demo) fixes that at INSTALL time — one banner, once. This
// package fixes it at CALL time, continuously: every call ends by naming the
// small set of RECOMMENDED next commands for where the agent now stands.
//
// # The model
//
// A graph is a flat list of Edges, not a node tree. An edge answers exactly one
// question: "the agent just ran FROM and it went ON — what now?" The graph
// structure is implicit in from→then, which keeps authoring, diffing, review and
// testing to a single flat list. Nodes would add a second thing to keep in sync
// with `exposes` and buy nothing.
//
// Crucially the recommendation is a function of (method, outcome) ONLY. There is
// no session state and nothing is remembered between calls. That is a deliberate
// choice: the app's own error IS the state signal. An agent that skipped a
// mandatory gateway (e.g. primitive.signup) finds out because the call it tried
// returned 401 — and the 401 edge names the gateway. Stateless matching means a
// graph is a pure, testable function, and it cannot go stale against the real
// account state the way a local "have I signed up?" flag would.
//
// # Recommended, not exhaustive
//
// A graph deliberately does NOT enumerate an app's methods — `<ns>.help` already
// does, and io.pilot.primitive exposes 110. A graph names only utility methods
// that advance the app's actual flow. Getters, listers and lifecycle management
// are reachable via help and must not crowd out the flow. maxThen caps every
// edge at three steps for exactly this reason: an agent with a small context
// window reads three commands, not thirty.
//
// # Trust and failure posture
//
// The graph is authored in submission.json next to product_demo, flows verbatim
// into the catalogue's metadata.json, and is pinned by metadata_sha256 under the
// catalogue signature — so it introduces no new trust surface. Everything is
// additive and best-effort: an absent, malformed, or unmatched graph means the
// call behaves exactly as it does today. This package is a leaf — it imports
// nothing from the rest of the tree, so publish and scaffold can both depend on
// it without a cycle.
package nextsteps

import (
	"fmt"
	"regexp"
	"strings"
)

// Budgets keep a graph small enough to survive a small context window and keep
// the per-call banner from becoming noise the agent learns to skip. They are
// enforced by Validate as hard errors.
const (
	// maxThen is the ceiling on recommendations per edge. Three is the point
	// where an agent still reads every line. An edge that wants more is really
	// documentation and belongs in <ns>.help.
	maxThen = 3
	// maxEdges bounds a whole graph. Hitting it means the graph is enumerating
	// methods rather than describing a flow.
	maxEdges   = 40
	maxWhy     = 200
	maxStepWhy = 160
)

// Wildcard is the From value matching any method. It is how an app says "no
// matter what you just called, if it failed like THIS, do this next" — the
// shape almost every recovery edge takes (401 → signup, 402 → top up).
const Wildcard = "*"

// Outcome values for Edge.On. There are exactly two, because `pilotctl appstore
// call` has exactly two observable outcomes: exit 0 with a JSON result, or exit
// 1 with an error on stderr. (fatalHint is the only failure path and it always
// exits 1 — the exit CODE carries no classification, so everything finer than
// ok/err must come from Match or Code.)
const (
	OutcomeOK  = "ok"
	OutcomeErr = "err"
)

// Step kinds. Advisory — they drive how the step is framed in the rendered
// banner, and let the CI gate reason about a graph's shape (e.g. every app with
// a mandatory gateway must have a KindGateway recovery edge).
const (
	// KindGateway is a method that MUST run before the app works at all
	// (primitive.signup, postgres.start, docker.engine_start).
	KindGateway = "gateway"
	// KindFlow is the normal forward path: the next useful thing to do.
	KindFlow = "flow"
	// KindRecovery undoes a specific failure (top up after 402, back off after
	// 429, add the missing param).
	KindRecovery = "recovery"
)

// Graph is an app's next-steps graph. It is optional on a submission; when
// present it is validated at submit time and copied verbatim into metadata.json.
type Graph struct {
	// Schema is the wire version. 1 is the only accepted value; a client that
	// sees a higher number must ignore the graph rather than guess.
	Schema int `json:"schema"`
	// App is the owning app id (io.pilot.<name>). It MUST equal the submission
	// id — this is what stops a graph pasted from another app shipping silently.
	App string `json:"app"`
	// Edges is the flat rule list, evaluated by Resolve. Order matters only as
	// the tie-breaker between equally specific edges.
	Edges []Edge `json:"edges"`
}

// Edge is one rule: "if the agent ran From and it went On (optionally matching
// Match/Code), recommend Then".
type Edge struct {
	// From is the method just called ("primitive.send"), or Wildcard for any.
	From string `json:"from"`
	// On is OutcomeOK or OutcomeErr.
	On string `json:"on"`
	// Match is a regex over the call's OUTCOME PAYLOAD: the error text when
	// On == OutcomeErr, and the JSON result body when On == OutcomeOK.
	//
	// Matching the success body is not a nicety — it is load-bearing. The
	// single most important case in the whole feature, "you must call
	// <ns>.signup before anything else works", is NOT an error: the scaffold's
	// requireKey wrapper (templates/main.go.tmpl) soft-fails an unauthenticated
	// call by returning exit 0 with
	//   {"ok":false,"needs_signup":true,"activate":"primitive.signup"}
	// so an outcome-only matcher would see a plain success and say nothing —
	// precisely to the cold agent who most needs the gateway named. Every
	// signup app (primitive, didit, insforge) shares that wrapper, so this is a
	// class, not a special case.
	//
	// On the error side it is how 402/401/429 are classified today: the HTTP
	// adapter formats failures as
	//   backend: POST /v1/run -> 402: {"error":"insufficient credit ..."}
	// so the status is present in the text even without a structured code field.
	Match string `json:"match,omitempty"`
	// Code is an exact backend HTTP status match. It is the FUTURE, structured
	// path: it fires only when the call surfaces a machine-readable status, and
	// is preferred over Match when both are present. Authoring both is the
	// intended migration shape — Code wins where available, Match carries the
	// apps that predate it. Only valid with On == OutcomeErr.
	Code int `json:"code,omitempty"`
	// Why is a one-line framing printed above the steps ("budget exhausted").
	Why string `json:"why,omitempty"`
	// Then is the 1..maxThen recommended commands.
	Then []Step `json:"then"`
}

// Step is one recommended command plus the reason to run it. Why is required:
// a bare command is a guess, and an agent that does not know WHY a step matters
// re-derives it or skips it.
type Step struct {
	Cmd  string `json:"cmd"`
	Why  string `json:"why"`
	Kind string `json:"kind,omitempty"`
}

var (
	idRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
	// callRe pulls (app-id, method) out of a well-formed recommended command.
	callRe = regexp.MustCompile(`^pilotctl appstore call\s+(\S+)\s+(\S+)`)
)

// NamespaceOf returns the method namespace for an app id: io.pilot.duckdb →
// "duckdb". It is the same derivation publish uses, kept here so the package
// stays a leaf.
func NamespaceOf(appID string) string {
	if i := strings.LastIndex(appID, "."); i >= 0 && i+1 < len(appID) {
		return appID[i+1:]
	}
	return appID
}

// ValidID reports whether an app id is well-formed reverse-DNS.
func ValidID(id string) bool { return idRe.MatchString(id) }

// ParseCall extracts the app id and method from a recommended command. ok is
// false when cmd is not an `appstore call` (a legitimate case — a step may point
// at another pilotctl verb, e.g. `pilotctl appstore install ...`).
func ParseCall(cmd string) (appID, method string, ok bool) {
	m := callRe.FindStringSubmatch(strings.TrimSpace(cmd))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Resolve returns the single best edge for a completed call, or nil when the
// graph has nothing to say (the common, silent case).
//
// method is what was just called, ok is whether it exited 0, and payload is the
// call's outcome payload — the error text when !ok, the JSON result body when
// ok. Matching is pure: the same inputs always select the same edge.
//
// Specificity decides, not file order. The governing rule:
//
//	AN EDGE THAT MATCHED THE ACTUAL SITUATION BEATS ONE THAT MERELY MATCHED
//	THE METHOD NAME.
//
// So every discriminated edge (one with a code/match that fired) outranks every
// undiscriminated one, however exact its `from`:
//
//	from exact + code   (7)   a named method AND a known status
//	*          + code   (6)
//	from exact + match  (5)
//	*          + match  (4)
//	from exact          (1)   "you called this method", nothing more
//	*                   (0)   the catch-all
//
// The gap between 4 and 1 is the important part, and it is not academic — it is
// a bug this code had. A signup app's gateway edge is `*` + match on
// {"needs_signup":true} (the soft-fail is exit 0, so it cannot key on the
// outcome). If a bare `from`-exact flow edge could outrank it, then a COLD agent
// calling primitive.send_email would be told "now read your inbox" instead of
// "sign up first" — the exact failure this feature exists to prevent, on the
// exact class of app it matters most for, and invisible to CI because both edges
// are individually valid.
//
// Ties are broken by first-in-file, which is stable and documented, so an author
// can always force a winner by ordering.
func (g *Graph) Resolve(method string, ok bool, payload string) *Edge {
	if g == nil || g.Schema != 1 {
		return nil
	}
	want := OutcomeErr
	if ok {
		want = OutcomeOK
	}
	best := -1
	bestScore := -1
	for i := range g.Edges {
		e := &g.Edges[i]
		if e.On != want {
			continue
		}
		score := 0
		switch e.From {
		case method:
			score++ // a named method is the WEAKEST signal — see the ordering above
		case Wildcard:
			// no bonus — the catch-all
		default:
			continue // names a different method
		}
		// A code/match edge only applies when it actually matches. An edge that
		// declares a discriminator and does not match is NOT a fallback — it is
		// simply not applicable, so it must not win on its From bonus alone.
		if e.Code != 0 {
			if !matchesCode(payload, e.Code) {
				continue
			}
			score += 6 // matched the situation, and pinned an exact status
		} else if e.Match != "" {
			re, err := regexp.Compile("(?i)" + e.Match)
			if err != nil || !re.MatchString(payload) {
				continue
			}
			score += 4 // matched the situation
		}
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	if best < 0 {
		return nil
	}
	return &g.Edges[best]
}

// statusRe finds the backend status in an adapter error, e.g.
//
//	backend: POST /v1/run -> 402: {"error":"insufficient credit ..."}
//
// This is the bridge between today's string errors and Edge.Code: it reads the
// status the HTTP adapter already prints. When apps grow a structured error
// code, this is the one function that changes.
var statusRe = regexp.MustCompile(`->\s*(\d{3})\b`)

func matchesCode(errText string, code int) bool {
	m := statusRe.FindStringSubmatch(errText)
	if m == nil {
		return false
	}
	return m[1] == fmt.Sprintf("%d", code)
}

// Validate reports the blocking problems with a graph. appID is the owning app
// id. exposes is the app's method list (from the signed manifest / submission);
// when non-empty, every in-app method named by an edge is checked against it —
// this is what makes a graph that recommends a method the app does not have a
// BUILD failure rather than a dead end an agent discovers at runtime.
func (g *Graph) Validate(appID string, exposes []string) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if g.Schema != 1 {
		add("next_steps.schema is %d; must be 1", g.Schema)
	}
	if g.App != appID {
		add("next_steps.app %q must equal the app id %q", g.App, appID)
	}
	ns := NamespaceOf(appID)

	known := map[string]bool{}
	for _, m := range exposes {
		known[m] = true
	}

	switch {
	case len(g.Edges) == 0:
		add("next_steps.edges is empty; a graph with no edges is not a graph — drop the field instead")
	case len(g.Edges) > maxEdges:
		add("next_steps.edges has %d; keep it to at most %d (a graph describes the flow, it does not enumerate methods — that is <ns>.help)", len(g.Edges), maxEdges)
	}

	seen := map[string]int{}
	for i := range g.Edges {
		g.Edges[i].validate(fmt.Sprintf("next_steps.edges[%d]", i), ns, known, &errs)
		// Two edges with the same (from, on, match, code) can never both fire —
		// the second is dead weight the author almost certainly did not intend.
		key := g.Edges[i].From + "|" + g.Edges[i].On + "|" + g.Edges[i].Match + "|" + fmt.Sprint(g.Edges[i].Code)
		if prev, dup := seen[key]; dup {
			add("next_steps.edges[%d] duplicates edges[%d] (same from/on/match/code); the second can never fire", i, prev)
		}
		seen[key] = i
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func (e *Edge) validate(where, ns string, known map[string]bool, errs *[]string) {
	add := func(f string, a ...any) { *errs = append(*errs, fmt.Sprintf(f, a...)) }

	switch e.From {
	case "":
		add("%s.from is required (a %s.* method, or %q for any)", where, ns, Wildcard)
	case Wildcard:
	default:
		if !strings.HasPrefix(e.From, ns+".") {
			add("%s.from %q must be a %s.* method or %q", where, e.From, ns, Wildcard)
		} else if len(known) > 0 && !known[e.From] {
			add("%s.from %q is not a method this app exposes", where, e.From)
		}
	}

	switch e.On {
	case OutcomeOK:
		// match IS valid here: it tests the success body, which is where a
		// soft-failed gateway ({"needs_signup":true}) announces itself. code is
		// not — only a failed call carries a backend status.
		if e.Code != 0 {
			add("%s.code is only valid with on:%q (a successful call has no error status)", where, OutcomeErr)
		}
		validateRegex(where, e.Match, errs)
	case OutcomeErr:
		validateRegex(where, e.Match, errs)
		if e.Code != 0 && (e.Code < 100 || e.Code > 599) {
			add("%s.code %d is not an HTTP status", where, e.Code)
		}
	case "":
		add("%s.on is required (%q or %q)", where, OutcomeOK, OutcomeErr)
	default:
		add("%s.on %q must be %q or %q", where, e.On, OutcomeOK, OutcomeErr)
	}

	if len(e.Why) > maxWhy {
		add("%s.why is %d chars; keep it under %d", where, len(e.Why), maxWhy)
	}

	switch {
	case len(e.Then) == 0:
		add("%s.then is empty; an edge that recommends nothing should be deleted", where)
	case len(e.Then) > maxThen:
		add("%s.then has %d steps; keep it to at most %d — an agent reads three commands, not %d", where, len(e.Then), maxThen, len(e.Then))
	}
	for i, s := range e.Then {
		s.validate(fmt.Sprintf("%s.then[%d]", where, i), ns, known, errs)
	}
}

// validateRegex reports an uncompilable match. A bad regex is silently inert at
// runtime (Resolve drops it), which is the worst failure mode there is: the
// author believes the recovery edge ships and the agent never sees it. Catching
// it at submit time is the whole point.
func validateRegex(where, match string, errs *[]string) {
	if match == "" {
		return
	}
	if _, err := regexp.Compile(match); err != nil {
		*errs = append(*errs, fmt.Sprintf("%s.match %q is not a valid regex: %v", where, match, err))
	}
}

func (s *Step) validate(where, ns string, known map[string]bool, errs *[]string) {
	add := func(f string, a ...any) { *errs = append(*errs, fmt.Sprintf(f, a...)) }

	cmd := strings.TrimSpace(s.Cmd)
	switch {
	case cmd == "":
		add("%s.cmd is required", where)
	case !strings.HasPrefix(cmd, "pilotctl "):
		add("%s.cmd %q must be a runnable pilotctl command so the agent can copy-paste it", where, cmd)
	default:
		// A step MAY point at another app — the 402 recovery path legitimately
		// recommends io.pilot.wallet. What it may not do is name a method in a
		// namespace that does not belong to the app id it calls, which is the
		// signature of a copy-paste error.
		if appID, method, ok := ParseCall(cmd); ok {
			if !ValidID(appID) {
				add("%s.cmd calls %q, which is not a valid app id", where, appID)
			} else if want := NamespaceOf(appID); !strings.HasPrefix(method, want+".") {
				add("%s.cmd calls %s but the method is %q; it must be a %s.* method", where, appID, method, want)
			} else if want == ns && len(known) > 0 && !known[method] {
				// Only checkable for THIS app — another app's method list is not
				// available here; the CI gate cross-checks those.
				add("%s.cmd recommends %q, which this app does not expose", where, method)
			}
		}
	}

	if strings.TrimSpace(s.Why) == "" {
		add("%s.why is required — a command with no reason is a guess the agent has to re-derive", where)
	} else if len(s.Why) > maxStepWhy {
		add("%s.why is %d chars; keep it under %d", where, len(s.Why), maxStepWhy)
	}

	switch s.Kind {
	case "", KindGateway, KindFlow, KindRecovery:
	default:
		add("%s.kind %q must be %q, %q or %q", where, s.Kind, KindGateway, KindFlow, KindRecovery)
	}
}

// GatewayMethods returns the methods this graph marks as mandatory gateways.
// The CI gate uses it to check that an app whose flow has a gateway actually
// carries a recovery edge naming it.
func (g *Graph) GatewayMethods() []string {
	if g == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range g.Edges {
		for _, s := range e.Then {
			if s.Kind != KindGateway {
				continue
			}
			if _, m, ok := ParseCall(s.Cmd); ok && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}
