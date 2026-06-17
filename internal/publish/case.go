package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Case is one publish request treated as a reviewable unit: the full structured
// Submission, the build result, the review status, and an append-only history.
// Stored as <root>/cases/<case-id>/{case.json, bundle.tar.gz, metadata.json}.
type Case struct {
	CaseID     string     `json:"case_id"` // <id>-<version>
	Status     Status     `json:"status"`
	Submission Submission `json:"submission"`
	Build      BuildInfo  `json:"build"`
	History    []Event    `json:"history"`
	CreatedAt  string     `json:"created_at"`
	UpdatedAt  string     `json:"updated_at"`
}

// BuildInfo records what the server built + signed for this case.
type BuildInfo struct {
	BundleName     string `json:"bundle_name"`
	BundleSHA256   string `json:"bundle_sha256"`
	Publisher      string `json:"publisher"`
	BundleBytes    int64  `json:"bundle_bytes"`
	InstalledBytes int64  `json:"installed_bytes"`
}

// Event is one entry in a case's audit trail.
type Event struct {
	At     string `json:"at"`
	Status Status `json:"status"`
	Note   string `json:"note,omitempty"`
}

// CaseStore persists cases on disk with a clean access API.
type CaseStore struct{ root string }

// NewCaseStore roots the store at <dir>/cases.
func NewCaseStore(dir string) (*CaseStore, error) {
	root := filepath.Join(dir, "cases")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &CaseStore{root: root}, nil
}

func (s *CaseStore) dir(id string) string { return filepath.Join(s.root, safeKey(id)) }

// Dir exposes a case's directory (for staging the bundle on publish).
func (s *CaseStore) Dir(caseID string) string { return s.dir(caseID) }

// Create writes a new pending case from a built submission + bundle artifacts.
// Re-submitting the same id+version is refused once approved (bump the version).
func (s *CaseStore) Create(sub Submission, b *Bundle, build BuildInfo) (*Case, error) {
	id := safeKey(sub.ID + "-" + sub.Version)
	if existing, err := s.Get(id); err == nil && existing.Status == StatusApproved {
		return nil, fmt.Errorf("%s v%s is already approved/published; bump the version", sub.ID, sub.Version)
	}
	d := s.dir(id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	if b != nil {
		if err := os.WriteFile(filepath.Join(d, b.TarballName), b.Tarball, 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(d, "metadata.json"), b.MetadataJSON, 0o644); err != nil {
			return nil, err
		}
		// submission.json is the payload the publish workflow consumes on approval.
		sj, _ := json.MarshalIndent(map[string]string{
			"id": sub.ID, "version": sub.Version, "namespace": sub.Namespace(),
			"description": sub.Description, "bundle": b.TarballName, "bundle_sha256": b.SHA256,
		}, "", "  ")
		if err := os.WriteFile(filepath.Join(d, "submission.json"), append(sj, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	c := &Case{
		CaseID: id, Status: StatusPending, Submission: sub, Build: build,
		History:   []Event{{At: now, Status: StatusPending, Note: "submitted"}},
		CreatedAt: now, UpdatedAt: now,
	}
	return c, s.write(c)
}

func (s *CaseStore) write(c *Case) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(s.dir(c.CaseID), "case.json"), append(b, '\n'), 0o644)
}

// Get loads one case.
func (s *CaseStore) Get(caseID string) (*Case, error) {
	b, err := os.ReadFile(filepath.Join(s.dir(caseID), "case.json"))
	if err != nil {
		return nil, err
	}
	var c Case
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SetStatus transitions a case and appends to its history.
func (s *CaseStore) SetStatus(caseID string, st Status, note string) (*Case, error) {
	c, err := s.Get(caseID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	c.Status = st
	c.UpdatedAt = now
	c.History = append(c.History, Event{At: now, Status: st, Note: note})
	return c, s.write(c)
}

// List returns all cases, newest first.
func (s *CaseStore) List() ([]*Case, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []*Case
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if c, err := s.Get(e.Name()); err == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
