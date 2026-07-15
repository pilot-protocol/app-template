package nextsteps

import (
	"strings"
	"testing"
)

// realX402Err is a VERBATIM adapter error for a 402. It is not invented for the
// test: internal/scaffold/templates/client_http.go.tmpl formats every non-2xx
// as `backend: %s %s -> %d: %s`, and internal/broker/broker.go writes that body
// on an exhausted budget. The whole 402 recovery path depends on this exact
// string shape, so it is pinned here — if the adapter's format changes, this
// test fails and tells us the graphs need re-matching.
const realX402Err = `ipc: server error: backend: POST /v1/run -> 402: {"error":"insufficient credit — per-user budget exhausted","credits_remaining":0}`

// realQuotaErr is the broker's 429, from broker.go's writeJSON(StatusTooManyRequests).
const realQuotaErr = `ipc: server error: backend: POST /v1/run -> 429: {"error":"per-caller quota exceeded"}`

// realMissingParamErr is a verbatim CLI-adapter error, captured live from
// `pilotctl appstore call io.pilot.sqlite sqlite.query '{"sql":"SELECT 42"}'`.
// It is the single most common way an agent's first real call fails.
const realMissingParamErr = `ipc: server error: backend: missing required param(s): database`

func graph() *Graph {
	return &Graph{
		Schema: 1,
		App:    "io.pilot.demoapp",
		Edges: []Edge{
			{From: Wildcard, On: OutcomeErr, Match: `401|no api key`, Why: "not signed up",
				Then: []Step{{Cmd: "pilotctl appstore call io.pilot.demoapp demoapp.signup '{}'", Why: "mint your key", Kind: KindGateway}}},
			{From: Wildcard, On: OutcomeErr, Code: 402, Why: "budget exhausted",
				Then: []Step{{Cmd: "pilotctl appstore call io.pilot.demoapp demoapp.balance '{}'", Why: "check balance", Kind: KindRecovery}}},
			{From: Wildcard, On: OutcomeOK, Why: "catch-all",
				Then: []Step{{Cmd: "pilotctl appstore call io.pilot.demoapp demoapp.help '{}'", Why: "see everything"}}},
			{From: "demoapp.signup", On: OutcomeOK, Why: "signed up",
				Then: []Step{{Cmd: "pilotctl appstore call io.pilot.demoapp demoapp.send '{}'", Why: "send your first", Kind: KindFlow}}},
			{From: "demoapp.send", On: OutcomeErr, Match: `missing required param`, Why: "bad args",
				Then: []Step{{Cmd: "pilotctl appstore call io.pilot.demoapp demoapp.help '{}'", Why: "check the schema", Kind: KindRecovery}}},
		},
	}
}

func TestResolvePrefersSpecificMethodOverWildcard(t *testing.T) {
	g := graph()
	// demoapp.signup succeeding must select its own edge, not the wildcard ok.
	e := g.Resolve("demoapp.signup", true, "")
	if e == nil || e.Why != "signed up" {
		t.Fatalf("want the demoapp.signup ok edge, got %+v", e)
	}
	// An unrelated method succeeding falls back to the wildcard.
	e = g.Resolve("demoapp.other", true, "")
	if e == nil || e.Why != "catch-all" {
		t.Fatalf("want the wildcard ok edge, got %+v", e)
	}
}

func TestResolveMatchesRealX402(t *testing.T) {
	g := graph()
	e := g.Resolve("demoapp.run", false, realX402Err)
	if e == nil || e.Why != "budget exhausted" {
		t.Fatalf("a real 402 adapter error must select the 402 edge, got %+v", e)
	}
}

// A code edge must not fire on a DIFFERENT status. This is the regression that
// would make every failure claim the budget was exhausted.
func TestResolveCodeDoesNotFireOnOtherStatus(t *testing.T) {
	g := graph()
	if e := g.Resolve("demoapp.run", false, realQuotaErr); e != nil && e.Code == 402 {
		t.Fatalf("429 must not select the 402 edge, got %+v", e)
	}
}

// An edge whose discriminator does not match must not win on its From bonus.
// Without the `continue` in Resolve, `demoapp.send` failing for an unrelated
// reason would still select the missing-param edge and print a wrong fix.
func TestResolveNonMatchingDiscriminatorIsNotAFallback(t *testing.T) {
	g := graph()
	e := g.Resolve("demoapp.send", false, "backend: connection refused")
	if e != nil {
		t.Fatalf("no edge should match an unrelated error, got %+v", e)
	}
}

func TestResolveRealMissingParamSelectsRecovery(t *testing.T) {
	g := graph()
	e := g.Resolve("demoapp.send", false, realMissingParamErr)
	if e == nil || e.Why != "bad args" {
		t.Fatalf("want the missing-param recovery edge, got %+v", e)
	}
}

// Case-insensitivity matters because apps word errors inconsistently
// ("No API key" vs "no api key").
func TestResolveMatchIsCaseInsensitive(t *testing.T) {
	g := graph()
	e := g.Resolve("demoapp.send", false, "ipc: server error: backend: GET /v1/me -> 401: No API Key")
	if e == nil || e.Why != "not signed up" {
		t.Fatalf("match must be case-insensitive, got %+v", e)
	}
}

func TestResolveSilentWhenNothingMatches(t *testing.T) {
	g := graph()
	if e := g.Resolve("demoapp.whatever", false, "some novel error"); e != nil {
		t.Fatalf("want silence, got %+v", e)
	}
}

func TestResolveIgnoresUnknownSchema(t *testing.T) {
	g := graph()
	g.Schema = 2
	if e := g.Resolve("demoapp.signup", true, ""); e != nil {
		t.Fatalf("a future schema must be ignored, not guessed at; got %+v", e)
	}
}

func TestResolveNilGraphIsSafe(t *testing.T) {
	var g *Graph
	if e := g.Resolve("x", true, ""); e != nil {
		t.Fatalf("nil graph must resolve to nil")
	}
}

func TestValidateAcceptsGoodGraph(t *testing.T) {
	g := graph()
	exposes := []string{"demoapp.signup", "demoapp.send", "demoapp.balance", "demoapp.help"}
	if err := g.Validate("io.pilot.demoapp", exposes); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}
}

// The gate that matters most: a graph must not recommend a method the app does
// not have. That is a dead end an agent would otherwise discover at runtime.
func TestValidateRejectsUnknownRecommendedMethod(t *testing.T) {
	g := graph()
	g.Edges[3].Then[0].Cmd = "pilotctl appstore call io.pilot.demoapp demoapp.nonexistent '{}'"
	err := g.Validate("io.pilot.demoapp", []string{"demoapp.signup", "demoapp.send", "demoapp.balance", "demoapp.help"})
	if err == nil || !strings.Contains(err.Error(), "does not expose") {
		t.Fatalf("want an unknown-method error, got %v", err)
	}
}

func TestValidateRejectsUnknownFromMethod(t *testing.T) {
	g := graph()
	g.Edges[3].From = "demoapp.ghost"
	err := g.Validate("io.pilot.demoapp", []string{"demoapp.signup", "demoapp.send", "demoapp.balance", "demoapp.help"})
	if err == nil || !strings.Contains(err.Error(), "not a method this app exposes") {
		t.Fatalf("want an unknown-from error, got %v", err)
	}
}

// Cross-app steps are legitimate — the 402 path recommends io.pilot.wallet —
// so they must pass validation even though wallet's methods are unknown here.
func TestValidateAllowsCrossAppStep(t *testing.T) {
	g := graph()
	g.Edges[1].Then[0] = Step{Cmd: "pilotctl appstore call io.pilot.wallet wallet.balance '{}'", Why: "top up", Kind: KindRecovery}
	if err := g.Validate("io.pilot.demoapp", []string{"demoapp.signup", "demoapp.send", "demoapp.balance", "demoapp.help"}); err != nil {
		t.Fatalf("cross-app recovery step rejected: %v", err)
	}
}

// ...but a cross-app step whose namespace does not match its app id is a
// copy-paste error and must fail.
func TestValidateRejectsMismatchedNamespace(t *testing.T) {
	g := graph()
	g.Edges[1].Then[0] = Step{Cmd: "pilotctl appstore call io.pilot.wallet duckdb.query '{}'", Why: "nonsense", Kind: KindRecovery}
	err := g.Validate("io.pilot.demoapp", nil)
	if err == nil || !strings.Contains(err.Error(), "must be a wallet.* method") {
		t.Fatalf("want a namespace-mismatch error, got %v", err)
	}
}

func TestValidateRejectsCodeOnSuccessEdge(t *testing.T) {
	g := graph()
	g.Edges[2].Code = 402
	err := g.Validate("io.pilot.demoapp", nil)
	if err == nil || !strings.Contains(err.Error(), "only valid with on:\"err\"") {
		t.Fatalf("want a code-on-success error, got %v", err)
	}
}

// realNeedsSignupBody is the VERBATIM result of
//
//	pilotctl appstore call io.pilot.primitive primitive.get_account '{}'
//
// on a host that has not signed up — captured live. Note the exit code is 0:
// the scaffold's requireKey wrapper soft-fails rather than erroring, so this
// SUCCEEDS. It is the reason Match tests the success body and not just error
// text; without that, the "you must sign up first" case — the whole reason the
// feature exists — would be invisible.
const realNeedsSignupBody = `{
  "activate": "primitive.signup",
  "message": "No API key on this host yet. Call primitive.signup once (no arguments required) to provision a free account and managed inbox — the key is then stored locally under ~/.pilot and injected on every call automatically; you never pass it.",
  "needs_signup": true,
  "ok": false
}`

func TestResolveMatchesNeedsSignupOnSuccessBody(t *testing.T) {
	g := &Graph{Schema: 1, App: "io.pilot.primitive", Edges: []Edge{
		{From: Wildcard, On: OutcomeOK, Match: `"needs_signup"\s*:\s*true`, Why: "no account on this host yet",
			Then: []Step{{Cmd: "pilotctl appstore call io.pilot.primitive primitive.signup '{}'", Why: "provision a free account + inbox", Kind: KindGateway}}},
		{From: Wildcard, On: OutcomeOK, Why: "catch-all",
			Then: []Step{{Cmd: "pilotctl appstore call io.pilot.primitive primitive.help '{}'", Why: "see everything"}}},
	}}
	// The soft-failed call EXITS 0 — it must still route to the gateway.
	e := g.Resolve("primitive.get_account", true, realNeedsSignupBody)
	if e == nil || e.Why != "no account on this host yet" {
		t.Fatalf("a needs_signup success body must select the gateway edge, got %+v", e)
	}
	if !strings.Contains(e.RenderCall(), "(required first)") {
		t.Fatal("the gateway must render as required first")
	}
	// A genuine result must NOT be mistaken for a soft-fail.
	e = g.Resolve("primitive.get_account", true, `{"ok":true,"inbox":"agent@x.primitive.email"}`)
	if e == nil || e.Why != "catch-all" {
		t.Fatalf("a real success must fall through to the catch-all, got %+v", e)
	}
}

func TestValidateAllowsMatchOnSuccessEdge(t *testing.T) {
	g := &Graph{Schema: 1, App: "io.pilot.demoapp", Edges: []Edge{
		{From: Wildcard, On: OutcomeOK, Match: `"needs_signup"\s*:\s*true`, Why: "signup first",
			Then: []Step{{Cmd: "pilotctl appstore call io.pilot.demoapp demoapp.signup '{}'", Why: "mint key", Kind: KindGateway}}},
	}}
	if err := g.Validate("io.pilot.demoapp", []string{"demoapp.signup"}); err != nil {
		t.Fatalf("match on a success edge is the signup case and must be allowed: %v", err)
	}
}

func TestValidateRejectsTooManySteps(t *testing.T) {
	g := graph()
	s := g.Edges[0].Then[0]
	g.Edges[0].Then = []Step{s, s, s, s}
	err := g.Validate("io.pilot.demoapp", nil)
	if err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Fatalf("want a step-budget error, got %v", err)
	}
}

func TestValidateRejectsStepWithNoWhy(t *testing.T) {
	g := graph()
	g.Edges[0].Then[0].Why = ""
	err := g.Validate("io.pilot.demoapp", nil)
	if err == nil || !strings.Contains(err.Error(), "why is required") {
		t.Fatalf("want a missing-why error, got %v", err)
	}
}

func TestValidateRejectsBadRegex(t *testing.T) {
	g := graph()
	g.Edges[0].Match = "([unclosed"
	err := g.Validate("io.pilot.demoapp", nil)
	if err == nil || !strings.Contains(err.Error(), "not a valid regex") {
		t.Fatalf("want a regex error, got %v", err)
	}
}

func TestValidateRejectsForeignApp(t *testing.T) {
	g := graph()
	err := g.Validate("io.pilot.other", nil)
	if err == nil || !strings.Contains(err.Error(), "must equal the app id") {
		t.Fatalf("want an app-mismatch error, got %v", err)
	}
}

func TestValidateRejectsDuplicateEdges(t *testing.T) {
	g := graph()
	g.Edges = append(g.Edges, g.Edges[0])
	err := g.Validate("io.pilot.demoapp", nil)
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("want a duplicate-edge error, got %v", err)
	}
}

func TestGatewayMethods(t *testing.T) {
	got := graph().GatewayMethods()
	if len(got) != 1 || got[0] != "demoapp.signup" {
		t.Fatalf("want [demoapp.signup], got %v", got)
	}
}

func TestRenderCallIsCompactAndDeterministic(t *testing.T) {
	g := graph()
	e := g.Resolve("demoapp.run", false, realX402Err)
	out := e.RenderCall()
	if out != e.RenderCall() {
		t.Fatal("render must be deterministic")
	}
	if n := strings.Count(out, "\n"); n > 4 {
		t.Fatalf("per-call render must stay tiny; got %d lines:\n%s", n, out)
	}
	if !strings.Contains(out, "budget exhausted") || !strings.Contains(out, "wallet") && !strings.Contains(out, "balance") {
		t.Fatalf("render must name the problem and the fix, got:\n%s", out)
	}
	if !strings.Contains(out, "(fixes the error above)") {
		t.Fatalf("a recovery step must be marked as the fix, got:\n%s", out)
	}
}

func TestRenderCallMarksGateway(t *testing.T) {
	g := graph()
	e := g.Resolve("demoapp.send", false, "backend: GET /v1/me -> 401: no api key")
	out := e.RenderCall()
	if !strings.Contains(out, "(required first)") {
		t.Fatalf("a gateway step must be marked required, got:\n%s", out)
	}
}

func TestRenderCallNilEdgeIsEmpty(t *testing.T) {
	var e *Edge
	if e.RenderCall() != "" {
		t.Fatal("nil edge must render empty")
	}
}

func TestRenderCallCapsSteps(t *testing.T) {
	e := &Edge{Why: "x", Then: []Step{
		{Cmd: "pilotctl a", Why: "1"}, {Cmd: "pilotctl b", Why: "2"},
		{Cmd: "pilotctl c", Why: "3"}, {Cmd: "pilotctl d", Why: "4"},
	}}
	if strings.Contains(e.RenderCall(), "pilotctl d") {
		t.Fatal("render must hard-cap at maxThen even if validation was bypassed")
	}
}

func TestParseCall(t *testing.T) {
	id, m, ok := ParseCall("pilotctl appstore call io.pilot.duckdb duckdb.query '{\"sql\":\"SELECT 1\"}'")
	if !ok || id != "io.pilot.duckdb" || m != "duckdb.query" {
		t.Fatalf("got %q %q %v", id, m, ok)
	}
	if _, _, ok := ParseCall("pilotctl appstore install io.pilot.duckdb"); ok {
		t.Fatal("install is not a call")
	}
}

func TestNamespaceOf(t *testing.T) {
	for in, want := range map[string]string{
		"io.pilot.duckdb":       "duckdb",
		"io.telepat.ideon-free": "ideon-free",
	} {
		if got := NamespaceOf(in); got != want {
			t.Errorf("NamespaceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderMarkdownGroupsAndNamesGateways(t *testing.T) {
	out := graph().RenderMarkdown()
	for _, want := range []string{"## What to run next", "**Run first:** `demoapp.signup`", "### After a successful call", "### When a call fails"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
}

// TestDiscriminatedEdgeBeatsBareExactFrom pins the governing precedence rule:
// an edge that matched the ACTUAL SITUATION beats one that merely matched the
// METHOD NAME.
//
// This is a regression test for a real bug. Scoring originally gave `from`-exact
// a bigger bonus than a match, so for any requireKey-wrapped method that also had
// a flow edge, the bare flow edge (from exact) SHADOWED the gateway edge
// (* + needs_signup match). A cold agent calling primitive.send_email was told
// "now read your inbox" instead of "sign up first" — the precise failure this
// feature exists to prevent, on the class of app it matters most for, and
// invisible to CI because both edges are individually valid.
func TestDiscriminatedEdgeBeatsBareExactFrom(t *testing.T) {
	g := &Graph{Schema: 1, App: "io.pilot.primitive", Edges: []Edge{
		{From: Wildcard, On: OutcomeOK, Match: `"needs_signup"\s*:\s*true`, Why: "GATEWAY",
			Then: []Step{{Cmd: "pilotctl appstore call io.pilot.primitive primitive.signup '{}'", Why: "sign up", Kind: KindGateway}}},
		{From: "primitive.send_email", On: OutcomeOK, Why: "FLOW",
			Then: []Step{{Cmd: "pilotctl appstore call io.pilot.primitive primitive.list_emails '{}'", Why: "read replies"}}},
	}}
	// A COLD agent: send_email soft-fails with exit 0 + a needs_signup body.
	if e := g.Resolve("primitive.send_email", true, `{"ok":false,"needs_signup":true,"activate":"primitive.signup"}`); e == nil || e.Why != "GATEWAY" {
		t.Fatalf("a cold agent must be routed to the gateway, not the flow; got %+v", e)
	}
	// A WARM agent: the same method, a real result — the flow edge is correct now.
	if e := g.Resolve("primitive.send_email", true, `{"id":"msg_1","status":"queued"}`); e == nil || e.Why != "FLOW" {
		t.Fatalf("a warm agent must get the flow edge; got %+v", e)
	}
}

// The full ordering, pinned. See Resolve's doc comment.
func TestResolvePrecedenceOrdering(t *testing.T) {
	mk := func(from, on, match string, code int, why string) Edge {
		return Edge{From: from, On: on, Match: match, Code: code, Why: why,
			Then: []Step{{Cmd: "pilotctl x", Why: "y"}}}
	}
	// Every edge below matches the same 402 error; only specificity separates them.
	all := []Edge{
		mk(Wildcard, OutcomeErr, "", 0, "wildcard-bare"),
		mk("app.run", OutcomeErr, "", 0, "exact-bare"),
		mk(Wildcard, OutcomeErr, "insufficient", 0, "wildcard-match"),
		mk("app.run", OutcomeErr, "insufficient", 0, "exact-match"),
		mk(Wildcard, OutcomeErr, "", 402, "wildcard-code"),
		mk("app.run", OutcomeErr, "", 402, "exact-code"),
	}
	// Peel the winner off one at a time; each should be the next-most specific.
	want := []string{"exact-code", "wildcard-code", "exact-match", "wildcard-match", "exact-bare", "wildcard-bare"}
	for _, w := range want {
		g := &Graph{Schema: 1, App: "io.pilot.app", Edges: all}
		e := g.Resolve("app.run", false, realX402Err)
		if e == nil || e.Why != w {
			t.Fatalf("want %q to win, got %+v", w, e)
		}
		// drop the winner and re-resolve
		var next []Edge
		for _, x := range all {
			if x.Why != w {
				next = append(next, x)
			}
		}
		all = next
	}
}
