package demoeval

import (
	"strings"
	"testing"

	"github.com/pilot-protocol/app-template/internal/demo"
)

// duckdbMethods mirrors the real io.pilot.duckdb surface (a subset) so first-call
// tests don't depend on the concurrently-edited submissions tree.
func duckdbMethods() MethodSet {
	return MethodSet{
		Namespace: "duckdb",
		Methods: map[string]MethodSpec{
			"duckdb.query": {Name: "duckdb.query", Params: map[string]ParamSpec{
				"database": {Name: "database", Required: true},
				"sql":      {Name: "sql", Required: true},
			}},
			"duckdb.version": {Name: "duckdb.version", Params: map[string]ParamSpec{}},
			"duckdb.exec":    {Name: "duckdb.exec", Passthrough: true, Params: map[string]ParamSpec{}},
		},
	}
}

// TestFirstCallValidDemo: a demo whose quickstart + examples use real methods with
// real params yields a reachable, valid, plausible first call and a high score.
func TestFirstCallValidDemo(t *testing.T) {
	d := &demo.Demo{
		Skill:     "io.pilot.duckdb",
		WhenToUse: "SQL locally.",
		Quickstart: demo.Step{
			Command: `pilotctl appstore call io.pilot.duckdb duckdb.query '{"database":":memory:","sql":"SELECT 42"}'`,
			Expect:  `{"rows":[[42]]}`,
		},
		Examples: []demo.Step{
			{Command: `pilotctl appstore call io.pilot.duckdb duckdb.version '{}'`, Expect: "1.5.4"},
			{Command: `pilotctl appstore call io.pilot.duckdb duckdb.exec '{"args":["-version"]}'`, Expect: "v"},
		},
	}
	r := SimulateFirstCall(d, duckdbMethods())
	t.Logf("valid: %+v", r)
	if !r.Reachable || !r.MethodValid || !r.ArgsPlausible {
		t.Errorf("valid demo should be reachable+valid+plausible, got %+v", r)
	}
	if r.Score < 0.9 {
		t.Errorf("valid demo first-call score should be >=0.9, got %.2f", r.Score)
	}
}

// TestFirstCallBogusMethod: a quickstart calling a method the app doesn't declare
// must fail (an agent copying it makes an invalid call).
func TestFirstCallBogusMethod(t *testing.T) {
	d := &demo.Demo{
		Skill: "io.pilot.duckdb",
		Quickstart: demo.Step{
			Command: `pilotctl appstore call io.pilot.duckdb duckdb.frobnicate '{"x":1}'`,
		},
		Examples: []demo.Step{
			{Command: `pilotctl appstore call io.pilot.duckdb duckdb.query '{"database":":memory:","sql":"SELECT 1"}'`},
		},
	}
	r := SimulateFirstCall(d, duckdbMethods())
	t.Logf("bogus method: %+v", r)
	if r.MethodValid {
		t.Error("bogus method should not be MethodValid")
	}
	if r.Score >= 0.7 {
		t.Errorf("bogus-method demo should score low, got %.2f", r.Score)
	}
	if len(r.Notes) == 0 {
		t.Error("expected explanatory notes")
	}
}

// TestFirstCallBogusArgKey: real method, invented arg key → not plausible.
func TestFirstCallBogusArgKey(t *testing.T) {
	d := &demo.Demo{
		Quickstart: demo.Step{
			Command: `pilotctl appstore call io.pilot.duckdb duckdb.query '{"query":"SELECT 1"}'`,
		},
		Examples: []demo.Step{{Command: `pilotctl appstore call io.pilot.duckdb duckdb.version '{}'`}},
	}
	r := SimulateFirstCall(d, duckdbMethods())
	t.Logf("bogus arg: %+v", r)
	if r.MethodValid == false {
		t.Error("method should be valid")
	}
	if r.ArgsPlausible {
		t.Error("invented key 'query' should make args implausible")
	}
}

// TestFirstCallMissingRequired: real method, real key, but a required param is
// omitted → args still plausible (no invented key) but a note flags the gap and
// the score is docked below a perfect call. Mirrors the golden duckdb demo, which
// omits the required `database`.
func TestFirstCallMissingRequired(t *testing.T) {
	d := &demo.Demo{
		Quickstart: demo.Step{
			Command: `pilotctl appstore call io.pilot.duckdb duckdb.query '{"sql":"SELECT 42"}'`,
		},
		Examples: []demo.Step{{Command: `pilotctl appstore call io.pilot.duckdb duckdb.version '{}'`}},
	}
	r := SimulateFirstCall(d, duckdbMethods())
	t.Logf("missing required: %+v", r)
	if !r.ArgsPlausible {
		t.Error("real key with a missing required param should still be plausible")
	}
	foundNote := false
	for _, n := range r.Notes {
		if strings.Contains(n, "required") && strings.Contains(n, "database") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("expected a missing-required note mentioning database, got %v", r.Notes)
	}
	if r.Score >= 1.0 {
		t.Errorf("missing-required should dock below 1.0, got %.2f", r.Score)
	}
}

// TestFirstCallPassthrough: a passthrough method accepts an {"args":[…]} payload
// without those keys being declared params.
func TestFirstCallPassthrough(t *testing.T) {
	d := &demo.Demo{
		Quickstart: demo.Step{Command: `pilotctl appstore call io.pilot.duckdb duckdb.exec '{"args":["-version"]}'`},
		Examples:   []demo.Step{{Command: `pilotctl appstore call io.pilot.duckdb duckdb.version '{}'`}},
	}
	r := SimulateFirstCall(d, duckdbMethods())
	if !r.ArgsPlausible {
		t.Errorf("passthrough args payload should be plausible, got %+v", r)
	}
}

// TestFirstCallEmptyMethodSet: a pointer submission (no methods) can't be verified.
func TestFirstCallEmptyMethodSet(t *testing.T) {
	d := &demo.Demo{Quickstart: demo.Step{Command: `pilotctl appstore call io.pilot.x x.y '{}'`}}
	r := SimulateFirstCall(d, MethodSet{Namespace: "x"})
	if r.Reachable || r.Score != 0 {
		t.Errorf("empty method set should yield an unverifiable result, got %+v", r)
	}
}

// TestAnalyzeCommandParsing checks the command parser on a payload with spaces and
// escaped quotes (the hard case).
func TestAnalyzeCommandParsing(t *testing.T) {
	cmd := `pilotctl appstore call io.pilot.duckdb duckdb.query '{"database":":memory:","sql":"SELECT country FROM read_csv_auto(\"/data/u.csv\") GROUP BY 1"}'`
	a := analyzeCommand(cmd, duckdbMethods())
	if !a.parsed || a.method != "duckdb.query" || !a.methodValid || !a.argsParsed {
		t.Fatalf("parse failed: %+v", a)
	}
	if len(a.unknownKeys) != 0 {
		t.Errorf("unexpected unknown keys: %v", a.unknownKeys)
	}
	if len(a.missingRequired) != 0 {
		t.Errorf("both required params present, got missing: %v", a.missingRequired)
	}
}
