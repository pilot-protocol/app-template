package demoeval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pilot-protocol/app-template/internal/publish"
)

// Summary is the fleet-level rollup over a submissions tree.
type Summary struct {
	Total       int      `json:"total"`        // submissions scanned
	WithDemo    int      `json:"with_demo"`    // submissions carrying a product_demo
	WithoutDemo int      `json:"without_demo"` // submissions with no demo (the coverage gap)
	MeanScore   float64  `json:"mean_score"`   // mean quality score over demo-bearing apps
	Threshold   float64  `json:"threshold"`    // the min-score gate applied
	Below       []string `json:"below"`        // app ids whose demo scores below Threshold
}

// ScoreSubmissionsDir scores every submissions/<id>/submission.json under dir. It
// returns one Report per submission: demo-bearing submissions get a full rubric
// score plus the first-call proxy (run against that app's own declared methods);
// submissions with no demo are returned with HasDemo=false and Score=0 so callers
// can see the coverage gap and Summarize can count with/without. Reports are
// sorted by ascending score (worst first) so the CLI surfaces problems on top.
func ScoreSubmissionsDir(dir string) ([]Report, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var reports []Report
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "submission.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // dirs without a submission.json (e.g. pointer-only bundles)
		}
		var s publish.Submission
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		appID := s.ID
		if appID == "" {
			appID = e.Name()
		}
		r := Score(s.ProductDemo, appID, s.Namespace())
		if s.ProductDemo != nil {
			fc := SimulateFirstCall(s.ProductDemo, MethodSetFromSubmission(s))
			r.FirstCall = &fc
		}
		reports = append(reports, r)
	}
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].HasDemo != reports[j].HasDemo {
			return !reports[i].HasDemo // no-demo (worst) first
		}
		return reports[i].Score < reports[j].Score
	})
	return reports, nil
}

// Summarize rolls a set of reports up into a Summary, flagging demo-bearing apps
// whose score is below threshold.
func Summarize(reports []Report, threshold float64) Summary {
	sum := Summary{Threshold: threshold}
	var acc float64
	for _, r := range reports {
		sum.Total++
		if !r.HasDemo {
			sum.WithoutDemo++
			continue
		}
		sum.WithDemo++
		acc += r.Score
		if r.Score < threshold {
			sum.Below = append(sum.Below, r.AppID)
		}
	}
	if sum.WithDemo > 0 {
		sum.MeanScore = round1(acc / float64(sum.WithDemo))
	}
	sort.Strings(sum.Below)
	return sum
}
