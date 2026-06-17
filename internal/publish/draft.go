package publish

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
)

// Step is one wizard panel: a title for the progress legend and the form field
// keys it owns (so a POST of that step replaces exactly those keys in the draft).
type Step struct {
	Slug  string
	Title string
	Keys  []string
}

// Steps is the ordered wizard. Review is rendered after the last data step.
// The "type" step (API vs CLI) comes before the backend step, so the backend
// and methods steps render the right fields for the chosen kind.
var Steps = []Step{
	{Slug: "identity", Title: "Identity", Keys: []string{"id", "app_version", "description"}},
	{Slug: "type", Title: "Type", Keys: []string{"backend_type"}},
	{Slug: "backend", Title: "Backend", Keys: []string{"backend_base_url", "backend_command", "header_name", "header_value"}},
	{Slug: "methods", Title: "Methods", Keys: []string{"method_name", "method_verb", "method_path", "method_args", "method_summary", "method_duration", "method_params"}},
	{Slug: "listing", Title: "Listing", Keys: []string{"display_name", "tagline", "vendor_name", "vendor_url", "vendor_contact", "homepage", "source_url", "license", "categories", "keywords"}},
}

// DraftStore persists in-progress wizard submissions (one JSON file per draft),
// so the form is saved server-side after every Next — survives reloads.
type DraftStore struct{ root string }

func NewDraftStore(root string) (*DraftStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &DraftStore{root: root}, nil
}

func (d *DraftStore) path(id string) string { return filepath.Join(d.root, safeKey(id)+".json") }

// New mints a fresh draft id and writes an empty draft.
func (d *DraftStore) New() (string, error) {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	return id, d.Save(id, url.Values{})
}

// Load returns the draft's form values (empty if the draft is unknown).
func (d *DraftStore) Load(id string) (url.Values, error) {
	b, err := os.ReadFile(d.path(id))
	if err != nil {
		return url.Values{}, err
	}
	var m map[string][]string
	if err := json.Unmarshal(b, &m); err != nil {
		return url.Values{}, err
	}
	return url.Values(m), nil
}

// Save persists the whole draft.
func (d *DraftStore) Save(id string, v url.Values) error {
	b, _ := json.MarshalIndent(map[string][]string(v), "", "  ")
	return os.WriteFile(d.path(id), b, 0o644)
}

// MergeStep replaces this step's keys in the draft with the posted values and
// saves — the "save after every Next" behaviour.
func (d *DraftStore) MergeStep(id string, step Step, posted url.Values) error {
	cur, err := d.Load(id)
	if err != nil {
		return err
	}
	for _, k := range step.Keys {
		delete(cur, k)
		if vals, ok := posted[k]; ok {
			cur[k] = vals
		}
	}
	return d.Save(id, cur)
}

// Delete removes a draft (after a successful submit).
func (d *DraftStore) Delete(id string) { _ = os.Remove(d.path(id)) }

// RowView is one method/header row, for pre-filling the repeat groups.
type RowView struct{ Name, Verb, Path, Args, Summary, Duration, Params, Value string }

// MethodRows reconstructs the method rows from a draft (always at least one).
func MethodRows(v url.Values) []RowView {
	names := v["method_name"]
	if len(names) == 0 {
		return []RowView{{Verb: "GET", Duration: "fast"}}
	}
	out := make([]RowView, len(names))
	for i := range names {
		out[i] = RowView{
			Name: names[i], Verb: at(v["method_verb"], i), Path: at(v["method_path"], i),
			Args: at(v["method_args"], i), Summary: at(v["method_summary"], i),
			Duration: orDefault(at(v["method_duration"], i), "fast"), Params: at(v["method_params"], i),
		}
		if out[i].Verb == "" {
			out[i].Verb = "GET"
		}
	}
	return out
}

// HeaderRows reconstructs the auth-header rows from a draft (always at least one).
func HeaderRows(v url.Values) []RowView {
	names := v["header_name"]
	if len(names) == 0 {
		return []RowView{{}}
	}
	out := make([]RowView, len(names))
	for i := range names {
		out[i] = RowView{Name: names[i], Value: at(v["header_value"], i)}
	}
	return out
}
