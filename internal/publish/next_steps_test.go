package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilot-protocol/app-template/internal/nextsteps"
)

// TestAllSubmissionNextStepsValid is the CI gate for next-steps graphs. The
// single most important thing it enforces is that a graph can never recommend a
// method the app does not expose: a dead-end hint is worse than no hint, because
// the agent burns a call AND loses trust in everything else the app tells it.
// Graphs are validated against each submission's own declared method list, so
// that failure is caught here rather than by an agent at runtime.
func TestAllSubmissionNextStepsValid(t *testing.T) {
	entries, err := os.ReadDir(submissionsDir)
	if err != nil {
		t.Skipf("no submissions dir: %v", err)
	}
	var withGraph, total int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(submissionsDir, e.Name(), "submission.json"))
		if err != nil {
			continue // dirs without a submission.json (e.g. pointer bundles)
		}
		total++
		var s Submission
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Errorf("%s: bad JSON: %v", e.Name(), err)
			continue
		}
		if s.NextSteps == nil {
			continue
		}
		withGraph++
		for _, msg := range s.Validate() {
			// Only fail on next-steps errors so unrelated pre-existing
			// submission issues don't block the graph backfill.
			if strings.HasPrefix(msg, "Next steps:") {
				t.Errorf("%s: %s", e.Name(), msg)
			}
		}
	}
	t.Logf("next-steps coverage: %d/%d submissions carry a graph", withGraph, total)
}

// TestSubmissionGatewaysAreReachable enforces the rule that motivates the whole
// feature. An app with a mandatory gateway (primitive.signup, postgres.start) is
// the app an agent is most likely to abandon: it calls a real method, gets back a
// bare "needs_signup" or "connection refused", and concludes the app is broken.
// So any graph that names a gateway MUST also carry a wildcard edge that routes a
// cold agent back to it — otherwise the gateway is only ever advertised to agents
// that already found it.
func TestSubmissionGatewaysAreReachable(t *testing.T) {
	entries, err := os.ReadDir(submissionsDir)
	if err != nil {
		t.Skipf("no submissions dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(submissionsDir, e.Name(), "submission.json"))
		if err != nil {
			continue
		}
		var s Submission
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if s.NextSteps == nil {
			continue
		}
		gateways := s.NextSteps.GatewayMethods()
		if len(gateways) == 0 {
			continue
		}
		if !hasWildcardEdgeTo(s.NextSteps, gateways) {
			t.Errorf("%s: names gateway(s) %v but has no wildcard error edge routing a cold agent to one; "+
				"an agent that skips the gateway will only see the raw error and give up",
				e.Name(), gateways)
		}
	}
}

// hasWildcardEdgeTo reports whether the graph has a from:"*" edge — of EITHER
// outcome — whose steps include one of the given gateway methods.
//
// Either outcome, deliberately. An earlier version of this gate accepted only
// on:"err", which excluded the single most important gateway edge in the whole
// feature: a signup app's soft-fail is exit 0 with {"needs_signup":true}, so its
// gateway edge is on:"ok" + match. Requiring on:"err" forced every signup app to
// invent a second, weaker 401 edge to satisfy CI — and a 401 is not even the
// same condition (signup is idempotent and keeps an existing key, so it does not
// repair a revoked key; it only helps when none was ever minted).
//
// What actually matters is that a COLD agent — one that never called the gateway
// — is routed to it from whatever it happened to call first. A wildcard edge of
// either outcome does that.
func hasWildcardEdgeTo(g *nextsteps.Graph, gateways []string) bool {
	want := map[string]bool{}
	for _, m := range gateways {
		want[m] = true
	}
	for _, e := range g.Edges {
		if e.From != nextsteps.Wildcard {
			continue
		}
		for _, s := range e.Then {
			if _, m, ok := nextsteps.ParseCall(s.Cmd); ok && want[m] {
				return true
			}
		}
	}
	return false
}

// TestSubmissionCrossAppStepsResolve closes the one hole Validate cannot: a step
// may legitimately point at ANOTHER app (the 402 path recommends io.pilot.wallet),
// and nextsteps.Validate has no way to know that app's methods. Here we DO — every
// submission is on disk — so cross-app recommendations are resolved for real.
func TestSubmissionCrossAppStepsResolve(t *testing.T) {
	entries, err := os.ReadDir(submissionsDir)
	if err != nil {
		t.Skipf("no submissions dir: %v", err)
	}

	// Build the method table for every app we know about.
	methods := map[string]map[string]bool{}
	var subs []Submission
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(submissionsDir, e.Name(), "submission.json"))
		if err != nil {
			continue
		}
		var s Submission
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		subs = append(subs, s)
		set := map[string]bool{}
		for _, m := range s.MethodNames() {
			set[m] = true
		}
		methods[s.ID] = set
	}

	for _, s := range subs {
		if s.NextSteps == nil {
			continue
		}
		for _, e := range s.NextSteps.Edges {
			for _, step := range e.Then {
				appID, method, ok := nextsteps.ParseCall(step.Cmd)
				if !ok || appID == s.ID {
					continue // not a call, or same-app (already covered by Validate)
				}
				known, haveApp := methods[appID]
				if !haveApp {
					// Not every live app has a submission here (some are
					// externally owned). Unknown app = unverifiable, not wrong.
					continue
				}
				if len(known) > 0 && !known[method] {
					t.Errorf("%s: recommends %s on %s, which that app does not expose",
						s.ID, method, appID)
				}
			}
		}
	}
}
