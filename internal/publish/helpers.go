package publish

import "strings"

// Status is a case's review state.
type Status string

const (
	StatusSubmitted   Status = "submitted"    // received + validated; not built yet (admin triggers the build)
	StatusBuilding    Status = "building"     // bundle building asynchronously
	StatusBuildFailed Status = "build_failed" // async build errored; see the case note
	StatusPending     Status = "pending"      // built + awaiting admin review
	StatusApproved    Status = "approved"
	StatusRejected    Status = "rejected"
)

// safeKey makes a filesystem-safe key from an id+version.
func safeKey(k string) string {
	return strings.NewReplacer("/", "_", "..", "_", " ", "_").Replace(k)
}

// orDefault returns s, or d when s is empty.
func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
