package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status is a submission's review state.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

// Submission is one stored publish request + its review state.
type Submission struct {
	Key         string `json:"key"` // <id>-<version>
	ID          string `json:"id"`
	Version     string `json:"version"`
	Namespace   string `json:"namespace"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	BundleSHA   string `json:"bundle_sha256"`
	Status      Status `json:"status"`
	Detail      string `json:"detail,omitempty"` // publish output / rejection reason
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Store persists submissions under a root dir, one dir per submission.
type Store struct{ root string }

func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) dir(key string) string { return filepath.Join(s.root, key) }

// Save writes the bundle + submission.json + metadata.json + status for a built
// submission, returning the record. Re-submitting the same id+version replaces it
// only if it is not already approved.
func (s *Store) Save(cfg specMeta, b *Bundle) (*Submission, error) {
	key := safeKey(cfg.ID + "-" + cfg.Version)
	d := s.dir(key)
	if existing, err := s.Get(key); err == nil && existing.Status == StatusApproved {
		return nil, fmt.Errorf("%s v%s is already approved/published; bump the version", cfg.ID, cfg.Version)
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(d, b.TarballName), b.Tarball, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(d, "metadata.json"), b.MetadataJSON, 0o644); err != nil {
		return nil, err
	}
	subJSON := map[string]string{
		"id": cfg.ID, "version": cfg.Version, "namespace": b.Namespace,
		"description": cfg.Description, "bundle": b.TarballName, "bundle_sha256": b.SHA256,
	}
	sj, _ := json.MarshalIndent(subJSON, "", "  ")
	if err := os.WriteFile(filepath.Join(d, "submission.json"), append(sj, '\n'), 0o644); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := &Submission{
		Key: key, ID: cfg.ID, Version: cfg.Version, Namespace: b.Namespace,
		DisplayName: cfg.DisplayName, Description: cfg.Description, BundleSHA: b.SHA256,
		Status: StatusPending, CreatedAt: now, UpdatedAt: now,
	}
	return rec, s.writeStatus(rec)
}

func (s *Store) writeStatus(rec *Submission) error {
	b, _ := json.MarshalIndent(rec, "", "  ")
	return os.WriteFile(filepath.Join(s.dir(rec.Key), "status.json"), append(b, '\n'), 0o644)
}

// Get returns one submission record.
func (s *Store) Get(key string) (*Submission, error) {
	b, err := os.ReadFile(filepath.Join(s.dir(safeKey(key)), "status.json"))
	if err != nil {
		return nil, err
	}
	var rec Submission
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// SetStatus updates status + detail.
func (s *Store) SetStatus(key string, st Status, detail string) (*Submission, error) {
	rec, err := s.Get(key)
	if err != nil {
		return nil, err
	}
	rec.Status = st
	rec.Detail = detail
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return rec, s.writeStatus(rec)
}

// List returns all submissions, newest first.
func (s *Store) List() ([]*Submission, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []*Submission
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if rec, err := s.Get(e.Name()); err == nil {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// SubmissionDir is the on-disk dir for a submission (bundle + json files), used
// by the publisher when triggering the workflow.
func (s *Store) SubmissionDir(key string) string { return s.dir(safeKey(key)) }

// specMeta is the minimal spec info Save needs (avoids importing scaffold here).
type specMeta struct {
	ID, Version, Description, DisplayName string
}

func safeKey(k string) string {
	return strings.NewReplacer("/", "_", "..", "_", " ", "_").Replace(k)
}
