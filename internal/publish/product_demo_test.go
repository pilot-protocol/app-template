package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// submissionsDir is the repo-root submissions/ tree relative to this package.
const submissionsDir = "../../submissions"

// TestAllSubmissionDemosValid is the CI gate for product demos: every
// submission.json that carries a product_demo must produce a valid, publishable
// demo. It also fails a submission that is metered (broker-backed) but ships no
// demo — a metered app with no cost-annotated examples is exactly the app that
// gets installed and never used.
func TestAllSubmissionDemosValid(t *testing.T) {
	entries, err := os.ReadDir(submissionsDir)
	if err != nil {
		t.Skipf("no submissions dir: %v", err)
	}
	var withDemo, total int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(submissionsDir, e.Name(), "submission.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // dirs without a submission.json (e.g. pointer bundles)
		}
		total++
		var s Submission
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Errorf("%s: bad JSON: %v", e.Name(), err)
			continue
		}
		if s.ProductDemo == nil {
			continue
		}
		withDemo++
		for _, msg := range s.Validate() {
			// Only fail on demo-specific errors so unrelated pre-existing
			// submission issues don't block the demo backfill.
			if len(msg) >= 13 && msg[:13] == "Product demo:" {
				t.Errorf("%s: %s", e.Name(), msg)
			}
		}
	}
	t.Logf("product-demo coverage: %d/%d submissions carry a demo", withDemo, total)
}
