package demo

import (
	"strings"
	"testing"
)

// localDemo is a valid non-metered (local CLI) demo used across tests.
func localDemo() *Demo {
	return &Demo{
		Skill:     "io.pilot.duckdb",
		WhenToUse: "When you need an in-process SQL/OLAP engine to query CSV/Parquet or crunch data without standing up a server.",
		Quickstart: Step{
			Goal:    "Run your first query",
			Command: `pilotctl appstore call io.pilot.duckdb duckdb.query '{"sql":"SELECT 42 AS answer"}'`,
			Expect:  `{"rows":[[42]]}`,
		},
		Examples: []Step{
			{Title: "Aggregate a CSV", Command: `pilotctl appstore call io.pilot.duckdb duckdb.query '{"sql":"SELECT count(*) FROM read_csv_auto('/data/x.csv')"}'`, Expect: `{"rows":[[1000]]}`},
			{Title: "Version", Command: `pilotctl appstore call io.pilot.duckdb duckdb.version '{}'`, Expect: `{"version":"1.5.4"}`},
		},
		Gotchas: []string{"Paths are relative to the app sandbox, not your CWD."},
		Next:    []string{"io.pilot.duckdb duckdb.help '{}' for every method"},
	}
}

// meteredDemo is a valid metered (broker) demo whose worked flow stays under budget.
func meteredDemo() *Demo {
	return &Demo{
		Skill:     "io.pilot.agentphone",
		Metered:   true,
		WhenToUse: "When your agent needs to place a real phone call or send an SMS/iMessage to a person.",
		Quickstart: Step{
			Goal:    "Check your account (free)",
			Command: `pilotctl appstore call io.pilot.agentphone agentphone.usage '{}'`,
			Expect:  `{"plan":"managed","credits_remaining":5000000}`,
			Cost:    "$0.00 (read)",
		},
		Examples: []Step{
			{Title: "Place a call", Command: `pilotctl appstore call io.pilot.agentphone agentphone.place_call '{"to":"+14155551234","systemPrompt":"Confirm the 7pm booking."}'`, Expect: `{"id":"call_..."}`, Cost: "$0.10"},
			{Title: "Send a text", Command: `pilotctl appstore call io.pilot.agentphone agentphone.send_message '{"to":"+14155551234","text":"On my way"}'`, Expect: `{"id":"msg_..."}`, Cost: "$0.02"},
		},
		Cost: &Cost{
			Unit:       "micro-USD (1000000 = $1.00)",
			FreeBudget: "$5.00 per Pilot user",
			HardCapUSD: 5.00,
			Operations: []CostOp{
				{Op: "agentphone.place_call", Price: "$0.10", Note: "per call"},
				{Op: "agentphone.send_message", Price: "$0.02", Note: "per SMS/iMessage"},
				{Op: "agentphone.buy_number", Price: "$3.00", Note: "per month"},
			},
			WorkedTotal:  "This demo spends $0.12 of your $5.00 budget.",
			CheckBalance: `pilotctl appstore call io.pilot.agentphone agentphone.usage '{}'`,
		},
		Gotchas: []string{"Use E.164 numbers (+14155551234). Never dial 911."},
		Next:    []string{"io.pilot.agentphone agentphone.help '{}'"},
	}
}

func TestValidateOK(t *testing.T) {
	if err := localDemo().Validate("io.pilot.duckdb", "duckdb"); err != nil {
		t.Fatalf("local demo should be valid: %v", err)
	}
	if err := meteredDemo().Validate("io.pilot.agentphone", "agentphone"); err != nil {
		t.Fatalf("metered demo should be valid: %v", err)
	}
}

func TestValidateFailures(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Demo)
		want string
	}{
		{"skill mismatch", func(d *Demo) { d.Skill = "io.pilot.wrong" }, "must equal the app id"},
		{"no when", func(d *Demo) { d.WhenToUse = "" }, "when_to_use is required"},
		{"long when", func(d *Demo) { d.WhenToUse = strings.Repeat("x", maxWhenToUse+1) }, "keep it under"},
		{"few examples", func(d *Demo) { d.Examples = d.Examples[:1] }, "at least"},
		{"bad prefix", func(d *Demo) { d.Quickstart.Command = "duckdb.query '{}'" }, "copy-pasteable"},
		{"wrong namespace", func(d *Demo) { d.Examples[0].Command = "pilotctl appstore call io.pilot.duckdb redis.get '{}'" }, "must call a duckdb.* method"},
		{"no quickstart cmd", func(d *Demo) { d.Quickstart.Command = "" }, "quickstart.command is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := localDemo()
			tc.mut(d)
			err := d.Validate("io.pilot.duckdb", "duckdb")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMeteredValidation(t *testing.T) {
	// metered but no cost block
	d := meteredDemo()
	d.Cost = nil
	if err := d.Validate("io.pilot.agentphone", "agentphone"); err == nil || !strings.Contains(err.Error(), "MUST show costs") {
		t.Fatalf("metered w/o cost must fail: %v", err)
	}

	// over budget: three $3.00 number buys = $9.00 > $5.00
	d = meteredDemo()
	d.Examples = []Step{
		{Title: "Buy A", Command: `pilotctl appstore call io.pilot.agentphone agentphone.buy_number '{}'`, Cost: "$3.00"},
		{Title: "Buy B", Command: `pilotctl appstore call io.pilot.agentphone agentphone.buy_number '{}'`, Cost: "$3.00"},
	}
	// quickstart $0.00 + 2×$3.00 = $6.00 > $5.00
	if err := d.Validate("io.pilot.agentphone", "agentphone"); err == nil || !strings.Contains(err.Error(), "over the $5.00") {
		t.Fatalf("over-budget flow must fail: %v", err)
	}

	// cost set on a non-metered demo
	d2 := localDemo()
	d2.Cost = &Cost{HardCapUSD: 5}
	if err := d2.Validate("io.pilot.duckdb", "duckdb"); err == nil || !strings.Contains(err.Error(), "metered is false") {
		t.Fatalf("cost on non-metered must fail: %v", err)
	}
}

func TestQuotaMeteredValid(t *testing.T) {
	// A request-quota broker (sixtyfour): metered, but priced in requests not $.
	// hard_cap_usd == 0 => no dollar sum, no per-step $ required.
	d := &Demo{
		Skill:      "io.pilot.sixtyfour",
		Metered:    true,
		WhenToUse:  "When you need to enrich a person or company from an email, name, or domain.",
		Quickstart: Step{Goal: "Find an email", Command: `pilotctl appstore call io.pilot.sixtyfour sixtyfour.find_email '{"name":"Ada","company":"acme.com"}'`, Expect: `{"email":"..."}`},
		Examples: []Step{
			{Title: "Enrich a person", Command: `pilotctl appstore call io.pilot.sixtyfour sixtyfour.people_intelligence '{"email":"a@acme.com"}'`},
			{Title: "Enrich a company", Command: `pilotctl appstore call io.pilot.sixtyfour sixtyfour.company_intelligence '{"domain":"acme.com"}'`},
		},
		Cost: &Cost{
			Unit:       "requests (50 free per Pilot user)",
			FreeBudget: "50 requests per Pilot user",
			HardCapUSD: 0,
			Operations: []CostOp{{Op: "all methods", Price: "1 request", Note: "managed key"}},
		},
	}
	if err := d.Validate("io.pilot.sixtyfour", "sixtyfour"); err != nil {
		t.Fatalf("quota-metered demo should be valid: %v", err)
	}
	// missing free_budget must fail
	d.Cost.FreeBudget = ""
	if err := d.Validate("io.pilot.sixtyfour", "sixtyfour"); err == nil || !strings.Contains(err.Error(), "free_budget is required") {
		t.Fatalf("missing free_budget must fail: %v", err)
	}
}

func TestWorkedCostUSD(t *testing.T) {
	total, dyn := meteredDemo().WorkedCostUSD()
	if dyn {
		t.Fatalf("agentphone demo is not dynamically priced")
	}
	if total < 0.11 || total > 0.13 {
		t.Fatalf("worked total = %.2f, want ~0.12", total)
	}

	d := meteredDemo()
	d.Examples[0].Cost = "dynamic — see cost.operations"
	_, dyn = d.WorkedCostUSD()
	if !dyn {
		t.Fatalf("expected dynamic flag")
	}
}

func TestRenderSkill(t *testing.T) {
	out := meteredDemo().RenderSkill("io.pilot.agentphone")
	for _, want := range []string{
		"---\nname: io.pilot.agentphone\n",
		"when_to_use:",
		"## Run this first",
		"agentphone.place_call",
		"## Cost",
		"$5.00 per Pilot user",
		"$0.10",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill render missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderInstallAndMarkdown(t *testing.T) {
	inst := localDemo().RenderInstall("io.pilot.duckdb")
	if !strings.Contains(inst, "Run this first") || !strings.Contains(inst, "duckdb.query") {
		t.Fatalf("install render incomplete:\n%s", inst)
	}
	// non-metered install must not print a Cost line
	if strings.Contains(inst, "Cost:") {
		t.Fatalf("non-metered install should not show cost")
	}
	md := meteredDemo().RenderMarkdown("io.pilot.agentphone")
	if !strings.Contains(md, "## Cost") || !strings.Contains(md, "| Operation | Price | Notes |") {
		t.Fatalf("markdown render missing cost table:\n%s", md)
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"io.pilot.duckdb", "io.telepat.ideon-free"} {
		if !ValidID(ok) {
			t.Fatalf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"duckdb", "IO.PILOT.X", ""} {
		if ValidID(bad) {
			t.Fatalf("%s should be invalid", bad)
		}
	}
}
