package demoeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilot-protocol/app-template/internal/demo"
)

// goldenDir holds the frozen example demos shipped for authors.
const goldenDir = "../../docs/product-demo"

func loadGolden(t *testing.T, name string) *demo.Demo {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var d demo.Demo
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
	return &d
}

// TestGoldenDemosScoreHigh: the shipped golden examples are the reference for a
// good demo — both must score well above the CI gate.
func TestGoldenDemosScoreHigh(t *testing.T) {
	cases := []struct {
		file, appID string
	}{
		{"example.local.json", "io.pilot.duckdb"},
		{"example.metered.json", "io.pilot.agentphone"},
	}
	for _, tc := range cases {
		d := loadGolden(t, tc.file)
		r := Score(d, tc.appID, nsFromID(tc.appID))
		t.Logf("%s: score=%.1f skill_bytes=%d", tc.appID, r.Score, len(d.RenderSkill(tc.appID)))
		for _, c := range r.Criteria {
			t.Logf("    %-16s %.1f/%.1f  %s", c.Name, c.Earned, c.Weight, strings.Join(c.Reasons, "; "))
		}
		if r.Score < 85 {
			t.Errorf("%s: golden demo should score >=85, got %.1f (issues: %v)", tc.appID, r.Score, r.Issues)
		}
		if !r.HasDemo {
			t.Errorf("%s: HasDemo should be true", tc.appID)
		}
	}
}

// TestMeteredGoldenCostCriterion: the metered golden must earn full cost-discipline
// credit (cost block, balance check, in-budget, every spend annotated).
func TestMeteredGoldenCostCriterion(t *testing.T) {
	d := loadGolden(t, "example.metered.json")
	r := Score(d, "io.pilot.agentphone", "agentphone")
	if !r.Metered {
		t.Fatal("expected metered")
	}
	for _, c := range r.Criteria {
		if c.Name == "cost_discipline" && !c.pass() {
			t.Errorf("metered golden lost cost-discipline points: %v", c.Reasons)
		}
	}
	if r.WorkedUSD > r.BudgetUSD {
		t.Errorf("worked $%.2f over budget $%.2f", r.WorkedUSD, r.BudgetUSD)
	}
}

// TestBareDemoScoresLow: a skeletal demo — bad prefix, no expects, no when_to_use,
// no next, metered-but-no-cost — must score far below the gate.
func TestBareDemoScoresLow(t *testing.T) {
	bare := &demo.Demo{
		Skill:      "io.pilot.wrongid",                             // != appID
		WhenToUse:  "",                                             // missing
		Metered:    true,                                           // metered but...
		Quickstart: demo.Step{Command: "curl https://example.com"}, // wrong prefix, no expect
		Examples: []demo.Step{
			{Command: "echo hi"}, // wrong prefix, no ns method
		},
		// no Cost, no Next, no gotchas
	}
	r := Score(bare, "io.pilot.foo", "foo")
	t.Logf("bare score=%.1f issues=%v", r.Score, r.Issues)
	if r.Score >= 40 {
		t.Errorf("bare demo should score < 40, got %.1f", r.Score)
	}
	if len(r.Issues) == 0 {
		t.Error("bare demo should surface issues")
	}
}

// TestNilDemoIsWorstCase: no demo at all is the zero case.
func TestNilDemoIsWorstCase(t *testing.T) {
	r := Score(nil, "io.pilot.foo", "foo")
	if r.HasDemo {
		t.Error("nil demo should have HasDemo=false")
	}
	if r.Score != 0 {
		t.Errorf("nil demo score should be 0, got %.1f", r.Score)
	}
	if len(r.Issues) == 0 {
		t.Error("nil demo should report an issue")
	}
}

// TestNonMeteredGetsCostCredit: a well-formed local (non-metered) demo earns full
// cost-discipline credit by design.
func TestNonMeteredGetsCostCredit(t *testing.T) {
	d := loadGolden(t, "example.local.json")
	r := Score(d, "io.pilot.duckdb", "duckdb")
	for _, c := range r.Criteria {
		if c.Name == "cost_discipline" && !c.pass() {
			t.Errorf("non-metered demo should get full cost credit, lost: %v", c.Reasons)
		}
	}
}

// TestWhenToUseSentenceCheck exercises the single-sentence rule.
func TestWhenToUseSentenceCheck(t *testing.T) {
	single := "When you need an in-process SQL engine to query files locally."
	multi := "Use this for SQL. It also does analytics. And more."
	if !isSingleSentence(single) {
		t.Error("expected single sentence to pass")
	}
	if isSingleSentence(multi) {
		t.Error("expected multi-sentence to fail")
	}
}

// TestScoreSubmissionsDirNoDemos: over the real submissions tree (none carry a
// demo yet), every submission is reported with HasDemo=false and the summary
// reflects the coverage gap.
func TestScoreSubmissionsDirCoverage(t *testing.T) {
	reports, err := ScoreSubmissionsDir("../../submissions")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(reports) == 0 {
		t.Fatal("expected some submissions")
	}
	sum := Summarize(reports, DefaultMinScore)
	t.Logf("coverage: %d total, %d with demo, %d without, mean=%.1f",
		sum.Total, sum.WithDemo, sum.WithoutDemo, sum.MeanScore)
	if sum.Total != sum.WithDemo+sum.WithoutDemo {
		t.Errorf("counts don't add up: %d != %d + %d", sum.Total, sum.WithDemo, sum.WithoutDemo)
	}
}
