package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilot-protocol/app-template/internal/broker"
)

// BrokerRegistrar records a managed app in the broker's registry on approval.
// It is the seam between publishing (approve a managed app) and the broker
// (route + meter that app). The master key itself is never handled here — ops
// sets it in the env var named by the entry's KeyEnv, out of band.
type BrokerRegistrar interface {
	Register(entry broker.AppEntry) error
}

// MasterKeyEnv is the env var name the broker reads the master key from for this
// submission, derived deterministically from the app id so ops knows what to set
// (e.g. io.pilot.sixtyfour -> SIXTYFOUR_MASTER_KEY).
func (s Submission) MasterKeyEnv() string {
	repl := strings.NewReplacer("-", "_", ".", "_")
	return strings.ToUpper(repl.Replace(s.Namespace())) + "_MASTER_KEY"
}

// BrokerEntry derives the broker registry entry for a managed submission:
// where to forward (Upstream), which env var holds the master key (KeyEnv),
// which header to inject it as (AuthHeader), and which method paths are allowed.
func (s Submission) BrokerEntry() broker.AppEntry {
	authHeader := "Authorization" // safe default if the submitter didn't name one
	for _, h := range s.Backend.Headers {
		if strings.TrimSpace(h.Name) != "" {
			authHeader = h.Name
			break
		}
	}
	var allow []string
	for _, m := range s.Methods {
		if p := strings.TrimSpace(m.HTTP.Path); p != "" {
			allow = append(allow, p)
		}
	}
	return broker.AppEntry{
		ID:         s.ID,
		Upstream:   strings.TrimRight(s.Backend.BaseURL, "/"),
		KeyEnv:     s.MasterKeyEnv(),
		AuthHeader: authHeader,
		Allow:      allow,
		Quota:      0, // unlimited until ops sets a per-caller cap
	}
}

// FileRegistrar persists managed-app entries to a JSON registry file (the
// broker's apps.json). Registration is idempotent by app id: re-approving the
// same app updates its entry in place. After writing, the broker picks it up on
// its next reload (SIGHUP) — no broker restart needed.
type FileRegistrar struct{ Path string }

func (f FileRegistrar) Register(entry broker.AppEntry) error {
	if f.Path == "" {
		return fmt.Errorf("broker registry path not configured")
	}
	entries, err := readEntries(f.Path)
	if err != nil {
		return err
	}
	replaced := false
	for i, e := range entries {
		if e.ID == entry.ID {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	return writeEntries(f.Path, entries)
}

func readEntries(path string) ([]broker.AppEntry, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var entries []broker.AppEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("broker registry %s: %w", path, err)
	}
	return entries, nil
}

func writeEntries(path string, entries []broker.AppEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic swap so the broker never reads a half-written file
}
