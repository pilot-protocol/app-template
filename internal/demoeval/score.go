// Package demoeval evaluates the "product demo" that ships with an app for one
// concrete purpose: predicting whether the network's ~250k autonomous agents —
// which INSTALL apps but rarely USE them — will make a correct first call after
// an install, driven only by the demo the harness injects as a SKILL.md.
//
// It exposes two POTENTIAL-uplift metrics (deterministic, no LLM required):
//
//   - Score — a rubric-based 0–100 QUALITY score with a per-criterion breakdown
//     and human-readable reasons. It answers "is this demo good enough, in shape,
//     to drive a correct first call?" It is purely syntactic/structural: it never
//     needs the app's method set.
//
//   - SimulateFirstCall (firstcall.go) — the FIRST-CALL PROXY. Given the app's
//     real declared methods+params, it parses the demo's own commands and checks
//     that a small-context agent copy-pasting them would hit a real method with
//     plausible arguments. This is the reproducible stand-in for "usage uplift":
//     a demo whose commands reference real methods with real args means an agent
//     copying it makes a valid first call; one that doesn't, fails.
//
// The ACTUAL-uplift metric (did install→first-call conversion actually improve
// after demos shipped?) is the province of scripts/demo-telemetry.sh, which pulls
// the broker/telemetry signal. See internal/demoeval/README.md.
//
// This package depends only on internal/demo (the frozen demo type) and
// internal/publish (the Submission type, for the method set) — never the reverse.
package demoeval

import (
	"strings"

	"github.com/pilot-protocol/app-template/internal/demo"
)

// callPrefix mirrors the (unexported) literal every runnable demo command must
// start with. Kept in sync with internal/demo by construction; the golden
// examples and demo.Validate enforce the same prefix.
const callPrefix = "pilotctl appstore call "

// DefaultMinScore is the default CI gate: a demo scoring below this (0–100) is
// treated as not good enough to reliably drive a first call.
const DefaultMinScore = 60.0

// maxSkillLen is the brevity target for the rendered SKILL.md (in bytes). A demo
// whose skill renders under this earns full brevity credit; credit falls linearly
// to zero at 2× this length. Calibrated against real authored demos (which cluster
// ~2.3–3.3 KB) so a rich but disciplined demo passes comfortably, while a demo that
// has ballooned into documentation loses points. Small-context agents pay for every
// byte injected into their context window, so brevity is a first-class signal.
const maxSkillLen = 3000

// Criterion weights (points out of 100). Documented rationale:
//
//   - quickstart (18): the single most important thing — one runnable first call
//     with an expected output is what converts an install into a first success.
//   - examples (16): 2–6 worked, real <ns>.* calls cover the app's core value.
//   - copyPaste (16): every command must be literally copy-pasteable (correct
//     prefix) and show its expected output, or the agent can't self-verify.
//   - brevity (14): the skill must survive a small context window.
//   - skill (14): skill==appID + a `next` pointer + a bounded gotcha list are what
//     make the injected SKILL.md wire up and stay scannable.
//   - metered (12): metered apps must show costs, annotate every spend, fit the
//     budget, and expose a balance check — or an agent burns the user's credits.
//   - whenToUse (10): a single-sentence disambiguator is what stops an agent
//     reaching for the wrong tool in the first place.
//
// They sum to 100.
const (
	wQuickstart = 18.0
	wExamples   = 16.0
	wCopyPaste  = 16.0
	wBrevity    = 14.0
	wSkill      = 14.0
	wMetered    = 12.0
	wWhenToUse  = 10.0
)

// Criterion is one scored rubric line: how many of its Weight points the demo
// Earned, with human-readable reasons for anything it lost (or notably passed).
type Criterion struct {
	Name    string   `json:"name"`
	Weight  float64  `json:"weight"`
	Earned  float64  `json:"earned"`
	Reasons []string `json:"reasons,omitempty"`
}

// pass reports whether the criterion earned (nearly) full marks.
func (c Criterion) pass() bool { return c.Earned >= c.Weight-1e-9 }

// Report is the full quality assessment of one app's demo.
type Report struct {
	AppID     string      `json:"app_id"`
	Namespace string      `json:"namespace"`
	HasDemo   bool        `json:"has_demo"`
	Metered   bool        `json:"metered"`
	Score     float64     `json:"score"` // 0–100
	Criteria  []Criterion `json:"criteria,omitempty"`
	// WorkedUSD is the flat dollar cost the demo's worked flow spends; BudgetUSD is
	// the per-user hard cap (0 for non-dollar/free apps). Both surfaced for the CLI.
	WorkedUSD float64 `json:"worked_usd"`
	BudgetUSD float64 `json:"budget_usd"`
	// FirstCall is the potential-uplift proxy, populated by ScoreSubmissionsDir
	// (which has the app's method set). Nil when scored without a method set.
	FirstCall *FirstCallResult `json:"first_call,omitempty"`
	// Issues is the flat list of the most important human-readable problems, drawn
	// from the criteria that lost points — what a publisher should fix first.
	Issues []string `json:"issues,omitempty"`
}

// Score computes the rubric-based 0–100 quality score for a demo. appID is the
// owning app id (io.pilot.<name>); ns its namespace (<name>). A nil demo yields a
// zero-score report flagged HasDemo=false — the worst case, since an app with no
// demo is exactly the app that gets installed and never used.
func Score(d *demo.Demo, appID, ns string) Report {
	if strings.TrimSpace(ns) == "" {
		ns = nsFromID(appID)
	}
	r := Report{AppID: appID, Namespace: ns, HasDemo: d != nil}
	if d == nil {
		r.Issues = []string{"no product_demo — this app ships with nothing to convert an install into a first call"}
		return r
	}
	r.Metered = d.Metered

	crits := []Criterion{
		scoreQuickstart(d, ns),
		scoreExamples(d, ns),
		scoreCopyPaste(d, ns),
		scoreBrevity(d, appID),
		scoreSkill(d, appID),
		scoreMetered(d),
		scoreWhenToUse(d),
	}
	var total, earned float64
	for _, c := range crits {
		total += c.Weight
		earned += c.Earned
	}
	r.Criteria = crits
	if total > 0 {
		r.Score = round1(earned / total * 100)
	}
	r.WorkedUSD, _ = d.WorkedCostUSD()
	if d.Cost != nil {
		r.BudgetUSD = d.Cost.HardCapUSD
	}
	r.Issues = collectIssues(crits)
	return r
}

// scoreQuickstart: one runnable first call (correct prefix + a real <ns>.* method)
// with an expected output.
func scoreQuickstart(d *demo.Demo, ns string) Criterion {
	c := Criterion{Name: "quickstart", Weight: wQuickstart}
	cmd := strings.TrimSpace(d.Quickstart.Command)
	switch {
	case cmd == "":
		c.Reasons = append(c.Reasons, "no quickstart command — an agent has no first call to run")
	case !strings.HasPrefix(cmd, callPrefix):
		c.Reasons = append(c.Reasons, "quickstart command does not start with the pilotctl call prefix (not copy-pasteable)")
	case !callsNamespace(cmd, ns):
		c.Reasons = append(c.Reasons, "quickstart does not invoke a "+ns+".* method")
	default:
		c.Earned += 12 // runnable, prefixed, right namespace
	}
	if strings.TrimSpace(d.Quickstart.Expect) != "" {
		c.Earned += 6
	} else {
		c.Reasons = append(c.Reasons, "quickstart has no expected output — an agent can't tell success from failure")
	}
	return c
}

// scoreExamples: 2–6 worked examples, each a real <ns>.* call.
func scoreExamples(d *demo.Demo, ns string) Criterion {
	c := Criterion{Name: "examples", Weight: wExamples}
	n := len(d.Examples)
	switch {
	case n == 0:
		c.Reasons = append(c.Reasons, "no worked examples")
	case n < 2:
		c.Earned += 3
		c.Reasons = append(c.Reasons, "only 1 example — provide 2–6 worked flows")
	case n > 6:
		c.Earned += 4
		c.Reasons = append(c.Reasons, "more than 6 examples — this is a demo, not the full reference; trim to ≤6")
	default:
		c.Earned += 7 // healthy count
	}
	if n > 0 {
		valid := 0
		for _, ex := range d.Examples {
			cmd := strings.TrimSpace(ex.Command)
			if strings.HasPrefix(cmd, callPrefix) && callsNamespace(cmd, ns) {
				valid++
			}
		}
		c.Earned += 9 * float64(valid) / float64(n)
		if valid < n {
			c.Reasons = append(c.Reasons, plural(n-valid, "example")+" do not invoke a real "+ns+".* method")
		}
	}
	return c
}

// scoreCopyPaste: every command is prefixed correctly and shows its expected output.
func scoreCopyPaste(d *demo.Demo, ns string) Criterion {
	c := Criterion{Name: "copy_pasteable", Weight: wCopyPaste}
	steps := allSteps(d)
	if len(steps) == 0 {
		c.Reasons = append(c.Reasons, "no commands to copy")
		return c
	}
	prefixed, withExpect := 0, 0
	for _, s := range steps {
		if strings.HasPrefix(strings.TrimSpace(s.Command), callPrefix) {
			prefixed++
		}
		if strings.TrimSpace(s.Expect) != "" {
			withExpect++
		}
	}
	total := float64(len(steps))
	c.Earned += 8 * float64(prefixed) / total
	c.Earned += 8 * float64(withExpect) / total
	if prefixed < len(steps) {
		c.Reasons = append(c.Reasons, plural(len(steps)-prefixed, "command")+" don't start with the call prefix")
	}
	if withExpect < len(steps) {
		c.Reasons = append(c.Reasons, plural(len(steps)-withExpect, "step")+" have no expected output")
	}
	return c
}

// scoreBrevity: the rendered SKILL.md must stay small enough for a small context.
func scoreBrevity(d *demo.Demo, appID string) Criterion {
	c := Criterion{Name: "brevity", Weight: wBrevity}
	n := len(d.RenderSkill(appID))
	switch {
	case n <= maxSkillLen:
		c.Earned = wBrevity
	case n >= 2*maxSkillLen:
		c.Reasons = append(c.Reasons, sprintf("rendered SKILL is %d bytes (>2× the %d target) — too big for a small context window", n, maxSkillLen))
	default:
		// linear fall-off between target and 2×target
		frac := 1 - float64(n-maxSkillLen)/float64(maxSkillLen)
		c.Earned = round1(wBrevity * frac)
		c.Reasons = append(c.Reasons, sprintf("rendered SKILL is %d bytes (over the %d target)", n, maxSkillLen))
	}
	return c
}

// scoreSkill: skill-file discipline — skill==appID, a `next` pointer, ≤6 gotchas.
func scoreSkill(d *demo.Demo, appID string) Criterion {
	c := Criterion{Name: "skill_discipline", Weight: wSkill}
	if d.Skill == appID {
		c.Earned += 6
	} else {
		c.Reasons = append(c.Reasons, sprintf("skill %q must equal the app id %q, or the injected SKILL.md won't line up", d.Skill, appID))
	}
	if len(d.Next) >= 1 {
		c.Earned += 5
	} else {
		c.Reasons = append(c.Reasons, "no `next` pointer (e.g. \"<ns>.help\") — an agent has nowhere to go deeper")
	}
	if len(d.Gotchas) <= 6 {
		c.Earned += 3
	} else {
		c.Reasons = append(c.Reasons, sprintf("%d gotchas — keep it to ≤6 so the list stays scannable", len(d.Gotchas)))
	}
	return c
}

// scoreMetered: cost discipline for metered apps. Non-metered apps earn full
// credit (they carry no cost obligations by design).
func scoreMetered(d *demo.Demo) Criterion {
	c := Criterion{Name: "cost_discipline", Weight: wMetered}
	if !d.Metered {
		c.Earned = wMetered
		c.Reasons = append(c.Reasons, "not metered — cost discipline N/A (full credit)")
		return c
	}
	if d.Cost == nil {
		c.Reasons = append(c.Reasons, "metered app with no cost block — an agent can't see what it's about to spend")
		return c
	}
	// The 12 points split evenly across the four cost obligations (3 each): a price
	// table, a balance check, an in-budget worked flow, and a cost annotation on
	// every spending step.
	// has a cost block with a price table
	if len(d.Cost.Operations) > 0 {
		c.Earned += 3
	} else {
		c.Reasons = append(c.Reasons, "cost.operations is empty — list the price of every spending op")
	}
	// a balance check the agent can run
	if strings.TrimSpace(d.Cost.CheckBalance) != "" {
		c.Earned += 3
	} else {
		c.Reasons = append(c.Reasons, "no check_balance command — an agent can't see its remaining budget")
	}
	// worked flow fits the budget
	total, hasDynamic := d.WorkedCostUSD()
	switch {
	case d.Cost.HardCapUSD <= 0:
		c.Earned += 3 // non-dollar meter (e.g. request quota): no dollar sum to check
	case total <= d.Cost.HardCapUSD+1e-9:
		c.Earned += 3
	default:
		c.Reasons = append(c.Reasons, sprintf("worked flow spends $%.2f, over the $%.2f budget", total, d.Cost.HardCapUSD))
	}
	// every $-spending step annotated with its cost
	if hasDynamic {
		c.Earned += 3 // dynamically-priced flow: per-step dollar annotation not expected
	} else {
		annotated, spend := 0, 0
		for _, s := range allSteps(d) {
			if strings.TrimSpace(s.Cost) == "" {
				spend++ // treat an un-annotated step as a potential silent spend
			} else {
				annotated++
			}
		}
		if spend == 0 {
			c.Earned += 3
		} else {
			c.Earned += 3 * float64(annotated) / float64(annotated+spend)
			c.Reasons = append(c.Reasons, plural(spend, "step")+" on a metered app carry no cost annotation")
		}
	}
	return c
}

// scoreWhenToUse: a single-sentence, in-budget disambiguator.
func scoreWhenToUse(d *demo.Demo) Criterion {
	c := Criterion{Name: "when_to_use", Weight: wWhenToUse}
	s := strings.TrimSpace(d.WhenToUse)
	if s == "" {
		c.Reasons = append(c.Reasons, "when_to_use is empty — nothing tells an agent WHEN to reach for this app")
		return c
	}
	c.Earned += 4
	if len(s) <= 240 {
		c.Earned += 3
	} else {
		c.Reasons = append(c.Reasons, sprintf("when_to_use is %d chars — keep it under 240 for small-context agents", len(s)))
	}
	if isSingleSentence(s) {
		c.Earned += 3
	} else {
		c.Reasons = append(c.Reasons, "when_to_use is more than one sentence — a single crisp sentence disambiguates best")
	}
	return c
}

// collectIssues flattens the reasons of every criterion that lost points, in
// rubric order, so the CLI can show "top issues" first.
func collectIssues(crits []Criterion) []string {
	var out []string
	for _, c := range crits {
		if c.pass() {
			continue
		}
		out = append(out, c.Reasons...)
	}
	return out
}
