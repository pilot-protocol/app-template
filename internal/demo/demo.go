// Package demo defines the "product demo" — a compact, example-driven, skill-file
// shaped usage guide that ships with every app. It is authored once in
// submission.json (the same place as descriptions and metadata), flows into the
// catalogue metadata.json, and is rendered three ways from this single source:
//
//   - RenderInstall — the block pilotctl prints at the last step of
//     `pilotctl appstore install io.pilot.<app>` (drive right-away usage).
//   - RenderMarkdown — the website "Full usage demo" section.
//   - RenderSkill   — the same demo in skill-file format (YAML frontmatter +
//     body), a library helper for agents/harnesses that consume skills. It is
//     NOT auto-injected anywhere; install-time rendering and the website are
//     the shipping surfaces.
//
// Unlike <ns>.help (which enumerates every capability), a demo is deliberately
// short and copy-pasteable: one first call, a handful of worked examples, and —
// for metered apps — an explicit, budget-checked cost breakdown. The package is a
// leaf: it imports nothing from the rest of the tree, so both publish and
// scaffold can depend on it without a cycle.
package demo

import (
	"fmt"
	"regexp"
	"strings"
)

// callPrefix is the literal every runnable example command must start with, so an
// agent copy-pastes something that actually works. Authored commands look like:
//
//	pilotctl appstore call io.pilot.duckdb duckdb.query '{"sql":"SELECT 42"}'
const callPrefix = "pilotctl appstore call "

// Length budgets keep a demo small enough to survive a small context window. They
// are enforced by Validate (hard errors) — a demo that blows the budget is not a
// demo, it's documentation, and belongs in <ns>.help / app_description.
const (
	maxWhenToUse = 240
	maxGotcha    = 240
	minExamples  = 2
	maxExamples  = 6
	maxGotchas   = 6
	maxNext      = 4
	maxNoteLen   = 400
)

// Demo is the structured product demo. It is optional on a submission; when
// present it is validated at submit time and rendered at install time and on the
// website. JSON tags match the `product_demo` object authors write in
// submission.json and that BuildMetadata copies verbatim into metadata.json.
type Demo struct {
	// Skill is the stable skill identifier. It MUST equal the app id
	// (io.pilot.<name>) so the injected SKILL.md and the app line up.
	Skill string `json:"skill"`
	// Title is the heading ("Full usage demo" by default) shown on the website
	// and at the top of the install banner.
	Title string `json:"title,omitempty"`
	// WhenToUse is a single sentence answering "WHEN should an agent reach for
	// this app?" — the disambiguator that stops an agent running the wrong tool.
	WhenToUse string `json:"when_to_use"`
	// Metered is true for apps behind a paying broker. When true, Cost is
	// required and the worked example costs are checked against the budget.
	Metered bool `json:"metered"`
	// Quickstart is the ONE call to run right now — the fastest path to a first
	// successful result.
	Quickstart Step `json:"quickstart"`
	// Examples are 2–6 worked, copy-pasteable flows that cover the app's core
	// value. Each is a real command with the output shape to expect.
	Examples []Step `json:"examples"`
	// Cost is the budget-checked cost breakdown. Required iff Metered.
	Cost *Cost `json:"cost,omitempty"`
	// Gotchas are ≤6 one-liners an agent must know before spending or before the
	// call fails in a surprising way.
	Gotchas []string `json:"gotchas,omitempty"`
	// Next points at deeper docs (typically "<ns>.help") — a pointer, never a
	// dump.
	Next []string `json:"next,omitempty"`
}

// Step is one runnable example: a goal, the exact command, the output to expect,
// and (for metered spending calls) the cost of running it.
type Step struct {
	Title   string `json:"title,omitempty"`
	Goal    string `json:"goal,omitempty"`
	Command string `json:"command"`
	Expect  string `json:"expect,omitempty"`
	// Cost is the price of running THIS step, e.g. "$0.10", "$0.00 (local)", or
	// "dynamic — see cost.operations". Required on metered spending steps.
	Cost string `json:"cost,omitempty"`
	Note string `json:"note,omitempty"`
}

// Cost is the per-app cost breakdown for a metered app. Prices are sourced from
// the deployed broker registry; HardCapUSD is the per-user-per-app ceiling
// (seed_credits) the worked example flow must stay under.
type Cost struct {
	Unit         string   `json:"unit"`                    // "micro-USD (1000000 = $1.00)"
	FreeBudget   string   `json:"free_budget"`             // "$5.00 per Pilot user"
	HardCapUSD   float64  `json:"hard_cap_usd"`            // 5.00 — machine-checkable ceiling
	Operations   []CostOp `json:"operations"`              // per-operation price table
	WorkedTotal  string   `json:"worked_total,omitempty"`  // human summary of what the demo spends
	CheckBalance string   `json:"check_balance,omitempty"` // command to read remaining balance
}

// CostOp is one row of the price table.
type CostOp struct {
	Op    string `json:"op"`             // method name, e.g. "agentphone.place_call"
	Price string `json:"price"`          // "$0.10", "$0.0432/cpu-hour", or "dynamic"
	Note  string `json:"note,omitempty"` // e.g. "reads are free"
}

var (
	idRe    = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
	priceRe = regexp.MustCompile(`\$([0-9]+(?:\.[0-9]+)?)`)
)

// TitleOr returns the demo title, defaulting to "Full usage demo".
func (d *Demo) TitleOr() string {
	if strings.TrimSpace(d.Title) != "" {
		return d.Title
	}
	return "Full usage demo"
}

// Validate reports the blocking problems with a demo. appID is the owning app id
// and ns its namespace (io.pilot.<name> → <name>). It returns nil when the demo
// is publishable. Callers join the returned errors; Submission.Validate surfaces
// the first. Non-blocking quality signals live in Lint (used by the scorer).
func (d *Demo) Validate(appID, ns string) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if d.Skill != appID {
		add("product_demo.skill %q must equal the app id %q", d.Skill, appID)
	}
	if s := strings.TrimSpace(d.WhenToUse); s == "" {
		add("product_demo.when_to_use is required (one sentence: when to reach for this app)")
	} else if len(s) > maxWhenToUse {
		add("product_demo.when_to_use is %d chars; keep it under %d for small-context agents", len(s), maxWhenToUse)
	}

	d.validateStep("product_demo.quickstart", ns, d.Quickstart, &errs)

	switch {
	case len(d.Examples) < minExamples:
		add("product_demo.examples has %d; need at least %d worked examples", len(d.Examples), minExamples)
	case len(d.Examples) > maxExamples:
		add("product_demo.examples has %d; keep it to at most %d (this is a demo, not the full reference)", len(d.Examples), maxExamples)
	}
	for i, ex := range d.Examples {
		d.validateStep(fmt.Sprintf("product_demo.examples[%d]", i), ns, ex, &errs)
	}

	if len(d.Gotchas) > maxGotchas {
		add("product_demo.gotchas has %d; keep it to at most %d", len(d.Gotchas), maxGotchas)
	}
	for i, g := range d.Gotchas {
		if len(g) > maxGotcha {
			add("product_demo.gotchas[%d] is %d chars; keep each under %d", i, len(g), maxGotcha)
		}
	}
	if len(d.Next) > maxNext {
		add("product_demo.next has %d; keep it to at most %d pointers", len(d.Next), maxNext)
	}

	d.validateCost(appID, &errs)

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func (d *Demo) validateStep(where, ns string, s Step, errs *[]string) {
	add := func(f string, a ...any) { *errs = append(*errs, fmt.Sprintf(f, a...)) }
	cmd := strings.TrimSpace(s.Command)
	if cmd == "" {
		add("%s.command is required", where)
		return
	}
	if !strings.HasPrefix(cmd, callPrefix) {
		add("%s.command must start with %q so it is copy-pasteable", where, callPrefix)
	}
	// The command must invoke a method in this app's namespace: "<ns>." must
	// appear after the call prefix. This catches examples pasted from another app.
	if ns != "" && !strings.Contains(cmd, " "+ns+".") {
		add("%s.command must call a %s.* method", where, ns)
	}
	if len(s.Note) > maxNoteLen {
		add("%s.note is %d chars; keep it under %d", where, len(s.Note), maxNoteLen)
	}
}

func (d *Demo) validateCost(appID string, errs *[]string) {
	add := func(f string, a ...any) { *errs = append(*errs, fmt.Sprintf(f, a...)) }
	if !d.Metered {
		if d.Cost != nil {
			add("product_demo.cost is set but metered is false; drop cost or set metered:true")
		}
		return
	}
	if d.Cost == nil {
		add("product_demo is metered but has no cost breakdown; metered apps MUST show costs")
		return
	}
	c := d.Cost
	if len(c.Operations) == 0 {
		add("product_demo.cost.operations is empty; list the price of every spending operation")
	}
	if strings.TrimSpace(c.Unit) == "" {
		add("product_demo.cost.unit is required (e.g. \"micro-USD (1000000 = $1.00)\" or \"requests (50 free per user)\")")
	}
	if strings.TrimSpace(c.FreeBudget) == "" {
		add("product_demo.cost.free_budget is required (e.g. \"$5.00 per Pilot user\" or \"50 requests per user\")")
	}
	// hard_cap_usd > 0 marks a DOLLAR-metered broker: the worked flow must fit the
	// budget, and every spending step must declare its cost. hard_cap_usd == 0 is a
	// non-dollar broker (e.g. a request quota) — Operations + FreeBudget still
	// describe the meter, but there is no dollar sum to check.
	if c.HardCapUSD > 0 {
		total, hasDynamic := d.WorkedCostUSD()
		if total > c.HardCapUSD+1e-9 {
			add("product_demo worked examples spend $%.2f, over the $%.2f per-user budget; trim the flow", total, c.HardCapUSD)
		}
		if !hasDynamic {
			for i, ex := range d.Examples {
				if strings.TrimSpace(ex.Cost) == "" {
					add("product_demo.examples[%d] on a $-metered app must declare its cost (e.g. \"$0.00 (read)\")", i)
				}
			}
		}
	}
}

// WorkedCostUSD sums the flat dollar costs of the quickstart + examples. It
// returns the total and whether any step is dynamically priced (excluded from the
// sum). A step whose cost has no "$n" amount (e.g. "dynamic", "free") contributes
// 0; a "dynamic" marker sets the second return.
func (d *Demo) WorkedCostUSD() (total float64, hasDynamic bool) {
	steps := append([]Step{d.Quickstart}, d.Examples...)
	for _, s := range steps {
		c := strings.ToLower(s.Cost)
		if strings.Contains(c, "dynamic") {
			hasDynamic = true
		}
		if m := priceRe.FindStringSubmatch(s.Cost); m != nil {
			var v float64
			fmt.Sscanf(m[1], "%f", &v)
			total += v
		}
	}
	return total, hasDynamic
}

// Valid reports whether the app id is well-formed reverse-DNS. Exposed for the
// scorer and tests.
func ValidID(id string) bool { return idRe.MatchString(id) }
