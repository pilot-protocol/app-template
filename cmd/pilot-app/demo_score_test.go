package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a well-formed submission: real methods + a demo whose commands hit them with
// valid params. Scores high and passes the gate.
const goodSubmission = `{
  "id": "io.pilot.demoapp",
  "version": "1.0.0",
  "description": "A demo app.",
  "email": "a@b.co",
  "backend": {"type": "cli", "command": ["demoapp"]},
  "methods": [
    {"name": "demoapp.query", "description": "run a query", "latency": "fast",
     "params": [{"name": "sql", "type": "string", "required": true}],
     "cli": {"args": ["-c", "${sql}"]}},
    {"name": "demoapp.version", "description": "version", "latency": "fast",
     "cli": {"args": ["--version"]}}
  ],
  "product_demo": {
    "skill": "io.pilot.demoapp",
    "when_to_use": "When you need to run a quick demo query locally.",
    "metered": false,
    "quickstart": {"goal": "first query",
      "command": "pilotctl appstore call io.pilot.demoapp demoapp.query '{\"sql\":\"SELECT 1\"}'",
      "expect": "{\"rows\":[[1]]}"},
    "examples": [
      {"title": "version", "command": "pilotctl appstore call io.pilot.demoapp demoapp.version '{}'", "expect": "1.0.0"},
      {"title": "count", "command": "pilotctl appstore call io.pilot.demoapp demoapp.query '{\"sql\":\"SELECT count(*)\"}'", "expect": "{\"rows\":[[3]]}"}
    ],
    "next": ["io.pilot.demoapp demoapp.help '{}'"]
  }
}`

// a broken submission: demo with the wrong command prefix, no expects, one
// example, metered-but-no-cost, mismatched skill. Scores below the gate.
const badSubmission = `{
  "id": "io.pilot.badapp",
  "version": "1.0.0",
  "description": "A bad app.",
  "email": "a@b.co",
  "backend": {"type": "cli", "command": ["badapp"]},
  "methods": [
    {"name": "badapp.go", "description": "go", "latency": "fast", "cli": {"args": ["go"]}}
  ],
  "product_demo": {
    "skill": "io.pilot.WRONG",
    "when_to_use": "",
    "metered": true,
    "quickstart": {"command": "curl https://example.com"},
    "examples": [{"command": "echo hi"}]
  }
}`

func writeSubmissions(t *testing.T, m map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range m {
		sub := filepath.Join(dir, name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "submission.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDemoScoreGood: a dir of only well-formed demos passes (exit 0) and shows a
// per-app row + summary.
func TestDemoScoreGood(t *testing.T) {
	dir := writeSubmissions(t, map[string]string{"io.pilot.demoapp": goodSubmission})
	var out, errOut bytes.Buffer
	code := runDemoScore([]string{dir}, &out, &errOut)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; err=%s", code, errOut.String())
	}
	s := out.String()
	if !strings.Contains(s, "io.pilot.demoapp") {
		t.Errorf("table missing app row:\n%s", s)
	}
	if !strings.Contains(s, "1 with demo") {
		t.Errorf("summary wrong:\n%s", s)
	}
}

// TestDemoScoreGateFails: a below-threshold demo trips the gate (exit 1) and is
// named in the failure line.
func TestDemoScoreGateFails(t *testing.T) {
	dir := writeSubmissions(t, map[string]string{
		"io.pilot.demoapp": goodSubmission,
		"io.pilot.badapp":  badSubmission,
	})
	var out, errOut bytes.Buffer
	code := runDemoScore([]string{dir}, &out, &errOut)
	if code != 1 {
		t.Fatalf("expected exit 1 (gate fail), got %d", code)
	}
	if !strings.Contains(errOut.String(), "io.pilot.badapp") {
		t.Errorf("failure line should name the bad app:\n%s", errOut.String())
	}
}

// TestDemoScoreMinFlag: raising -min above even a good demo trips the gate; a
// -min of 0 always passes.
func TestDemoScoreMinFlag(t *testing.T) {
	dir := writeSubmissions(t, map[string]string{"io.pilot.demoapp": goodSubmission})
	var out, errOut bytes.Buffer
	if code := runDemoScore([]string{"-min", "0", dir}, &out, &errOut); code != 0 {
		t.Errorf("min 0 should pass, got %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := runDemoScore([]string{"-min", "101", dir}, &out, &errOut); code != 1 {
		t.Errorf("min 101 should fail every demo, got %d", code)
	}
}

// TestDemoScoreJSON: -json emits machine-readable output.
func TestDemoScoreJSON(t *testing.T) {
	dir := writeSubmissions(t, map[string]string{"io.pilot.demoapp": goodSubmission})
	var out, errOut bytes.Buffer
	runDemoScore([]string{"-json", dir}, &out, &errOut)
	s := out.String()
	if !strings.Contains(s, `"reports"`) || !strings.Contains(s, `"summary"`) {
		t.Errorf("json output malformed:\n%s", s)
	}
}

// TestDemoScoreLiveTree: runs over the repo's real submissions tree — must scan
// without error (exit code != 2). Score/gate outcome isn't asserted because the
// tree is authored concurrently.
func TestDemoScoreLiveTree(t *testing.T) {
	if _, err := os.Stat("../../submissions"); err != nil {
		t.Skip("no submissions tree")
	}
	var out, errOut bytes.Buffer
	if code := runDemoScore([]string{"-min", "0", "../../submissions"}, &out, &errOut); code == 2 {
		t.Fatalf("scan error over live tree: %s", errOut.String())
	}
}
