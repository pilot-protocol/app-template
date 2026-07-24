package demoeval

import (
	"fmt"
	"math"
	"strings"

	"github.com/pilot-protocol/app-template/internal/demo"
)

// sprintf is a local alias so the scoring code reads without an fmt. prefix on
// every reason string.
func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// nsFromID derives the namespace (<name>) from an app id (io.pilot.<name>).
func nsFromID(id string) string {
	if i := strings.LastIndexByte(id, '.'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// callsNamespace reports whether a runnable command invokes a method in ns —
// i.e. " <ns>." appears after the call prefix, matching demo.Validate's rule.
func callsNamespace(cmd, ns string) bool {
	if ns == "" {
		return false
	}
	return strings.Contains(cmd, " "+ns+".")
}

// allSteps returns the quickstart followed by the examples, in order.
func allSteps(d *demo.Demo) []demo.Step {
	return append([]demo.Step{d.Quickstart}, d.Examples...)
}

// isSingleSentence reports whether s is a single sentence: no interior sentence
// terminator (".", "!", "?" followed by a space) and no newline. A single trailing
// terminator is fine.
func isSingleSentence(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, "\n\r") {
		return false
	}
	core := strings.TrimRight(s, ".!?")
	for _, sep := range []string{". ", "! ", "? "} {
		if strings.Contains(core, sep) {
			return false
		}
	}
	return true
}

// plural renders "N thing" / "N things" for a reason string.
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// round1 rounds to one decimal place so scores read cleanly.
func round1(f float64) float64 { return math.Round(f*10) / 10 }
