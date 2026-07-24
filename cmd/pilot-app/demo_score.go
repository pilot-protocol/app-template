package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/pilot-protocol/app-template/internal/demoeval"
)

// cmdDemoScore is the CLI entry point for `pilot-app demo-score`. It scores every
// submission's product demo (the potential-uplift report + CI gate) and exits
// nonzero when any demo scores below the -min threshold.
func cmdDemoScore(args []string) {
	os.Exit(runDemoScore(args, os.Stdout, os.Stderr))
}

// runDemoScore is the testable core: it prints the per-app table + summary to out
// (errors to errOut) and returns the process exit code. It never calls os.Exit,
// so tests can assert on the code and output.
func runDemoScore(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("demo-score", flag.ContinueOnError)
	fs.SetOutput(errOut)
	min := fs.Float64("min", demoeval.DefaultMinScore, "fail if any demo scores below this (0–100)")
	asJSON := fs.Bool("json", false, "emit the reports as JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := fs.Arg(0)
	if dir == "" {
		dir = "submissions"
	}

	reports, err := demoeval.ScoreSubmissionsDir(dir)
	if err != nil {
		fmt.Fprintf(errOut, "demo-score: %v\n", err)
		return 2
	}
	sum := demoeval.Summarize(reports, *min)

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Reports []demoeval.Report `json:"reports"`
			Summary demoeval.Summary  `json:"summary"`
		}{reports, sum})
		if len(sum.Below) > 0 {
			return 1
		}
		return 0
	}

	printTable(out, reports, *min)
	printSummary(out, sum)

	if len(sum.Below) > 0 {
		fmt.Fprintf(errOut, "\nDEMO-SCORE FAILED — %s below the %.0f threshold: %s\n",
			plur(len(sum.Below), "demo"), *min, strings.Join(sum.Below, ", "))
		return 1
	}
	return 0
}

// printTable renders the per-app potential-uplift table: quality score, first-call
// proxy, metered flag, worked-$ vs budget, and the top issue.
func printTable(out io.Writer, reports []demoeval.Report, min float64) {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tSCORE\t1ST-CALL\tMETERED\tWORKED$/BUDGET\tTOP ISSUE")
	fmt.Fprintln(tw, "---\t-----\t--------\t-------\t--------------\t---------")
	for _, r := range reports {
		score := "  n/a"
		if r.HasDemo {
			mark := "  "
			if r.Score < min {
				mark = "✗ "
			}
			score = fmt.Sprintf("%s%5.1f", mark, r.Score)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.AppID, score, firstCallCol(r), meteredCol(r), costCol(r), topIssue(r))
	}
	tw.Flush()
}

func firstCallCol(r demoeval.Report) string {
	if !r.HasDemo {
		return "—"
	}
	if r.FirstCall == nil || r.FirstCall.Skipped {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", r.FirstCall.Score)
}

func meteredCol(r demoeval.Report) string {
	if !r.HasDemo {
		return "—"
	}
	if r.Metered {
		return "✓"
	}
	return "-"
}

func costCol(r demoeval.Report) string {
	if !r.HasDemo || !r.Metered || r.BudgetUSD <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f/$%.2f", r.WorkedUSD, r.BudgetUSD)
}

// topIssue returns the single most important human-readable issue, truncated.
func topIssue(r demoeval.Report) string {
	if !r.HasDemo {
		return "no product_demo"
	}
	if len(r.Issues) == 0 {
		if r.FirstCall != nil && len(r.FirstCall.Notes) > 0 && r.FirstCall.Score < 1.0 {
			return truncate(r.FirstCall.Notes[0], 60)
		}
		return "ok"
	}
	return truncate(r.Issues[0], 60)
}

func printSummary(out io.Writer, s demoeval.Summary) {
	fmt.Fprintf(out, "\n%d submissions · %d with demo · %d without · mean score %.1f (gate %.0f)\n",
		s.Total, s.WithDemo, s.WithoutDemo, s.MeanScore, s.Threshold)
	if s.WithoutDemo > 0 {
		fmt.Fprintf(out, "coverage gap: %d app(s) ship no demo — installs that never convert to a first call\n", s.WithoutDemo)
	}
	if len(s.Below) > 0 {
		sort.Strings(s.Below)
		fmt.Fprintf(out, "below gate: %s\n", strings.Join(s.Below, ", "))
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func plur(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
