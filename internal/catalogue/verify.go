// Package catalogue verifies app bundles and catalogue entries against the same
// rules the pilot daemon enforces at install/spawn — reusing app-store/pkg/manifest
// so CI can't drift from the runtime. This is the objective half of the review
// gate (SPEC §7.1).
package catalogue

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pilot-protocol/app-store/pkg/manifest"
)

// Catalogue is the top-level index schema (catalogue/catalogue.json).
type Catalogue struct {
	Version   int     `json:"version"`
	UpdatedAt string  `json:"updated_at"`
	Apps      []Entry `json:"apps"`
}

// Entry is one app in the catalogue.
type Entry struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	Description  string `json:"description"`
	BundleURL    string `json:"bundle_url"`
	BundleSHA256 string `json:"bundle_sha256"`
}

// Result accumulates the per-entry verdict.
type Result struct {
	ID     string
	Checks []Check
}

// Check is one pass/fail line with a human message.
type Check struct {
	Name string
	OK   bool
	Msg  string
}

// OK reports whether every check passed.
func (r Result) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func (r *Result) pass(name, msg string) { r.Checks = append(r.Checks, Check{name, true, msg}) }
func (r *Result) fail(name, msg string) { r.Checks = append(r.Checks, Check{name, false, msg}) }
func (r *Result) check(name string, ok bool, okMsg, failMsg string) bool {
	if ok {
		r.pass(name, okMsg)
	} else {
		r.fail(name, failMsg)
	}
	return ok
}

// VerifyEntry runs every objective check for one catalogue entry (SPEC §7.1):
// download, tarball-sha, bundle contents, binary-sha, manifest Validate +
// VerifySignature, help-in-exposes, id/version consistency. prev is the entry
// being replaced (for the downgrade check), or nil.
func VerifyEntry(e Entry, prev *Entry) Result {
	r := Result{ID: e.ID}

	raw, err := fetch(e.BundleURL)
	if !r.check("bundle_url resolves", err == nil, e.BundleURL, fmt.Sprintf("fetch %s: %v", e.BundleURL, err)) {
		return r
	}

	sum := sha256.Sum256(raw)
	gotSHA := hex.EncodeToString(sum[:])
	r.check("bundle_sha256 matches", strings.EqualFold(gotSHA, e.BundleSHA256),
		gotSHA, fmt.Sprintf("got %s, catalogue says %s", gotSHA, e.BundleSHA256))

	mfRaw, binBytes, err := extractBundle(raw)
	if !r.check("bundle contains manifest.json + bin/<binary>", err == nil, "", fmt.Sprintf("%v", err)) {
		return r
	}

	m, err := manifest.Parse(mfRaw)
	if !r.check("manifest parses", err == nil, "", fmt.Sprintf("%v", err)) {
		return r
	}

	binSum := sha256.Sum256(binBytes)
	binSHA := hex.EncodeToString(binSum[:])
	r.check("binary.sha256 pin matches binary", strings.EqualFold(binSHA, m.Binary.SHA256),
		binSHA, fmt.Sprintf("manifest pins %s, binary is %s", m.Binary.SHA256, binSHA))

	if errs := m.Validate(); len(errs) == 0 {
		r.pass("manifest Validate()", "schema valid")
	} else {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		r.fail("manifest Validate()", strings.Join(msgs, "; "))
	}

	r.check("signature verifies", m.VerifySignature() == nil,
		"signed by "+short(m.Store.Publisher), errString(m.VerifySignature()))

	r.check("exposes a <ns>.help method", hasHelp(m.Exposes),
		"discovery contract satisfied", "no *.help method in exposes (SPEC §5.4)")

	r.check("catalogue.id == manifest.id", e.ID == m.ID, e.ID, fmt.Sprintf("catalogue %q != manifest %q", e.ID, m.ID))
	r.check("catalogue.version == manifest.app_version", e.Version == m.AppVersion,
		e.Version, fmt.Sprintf("catalogue %q != manifest %q", e.Version, m.AppVersion))

	if prev != nil {
		r.check("not a version downgrade", compareSemver(e.Version, prev.Version) >= 0,
			fmt.Sprintf("%s ≥ %s", e.Version, prev.Version),
			fmt.Sprintf("downgrade: %s < existing %s", e.Version, prev.Version))
	}
	return r
}

// EntryForBundle builds a catalogue Entry for a local bundle tarball: id and
// version are read from the bundle's manifest, the sha is computed, and the URL
// is the local file:// path. Lets `pilot-app verify <bundle>` run the full
// VerifyEntry pipeline on an unpublished bundle.
func EntryForBundle(path string) (Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	sum := sha256.Sum256(raw)
	mfRaw, _, err := extractBundle(raw)
	if err != nil {
		return Entry{}, err
	}
	var m struct {
		ID          string `json:"id"`
		AppVersion  string `json:"app_version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(mfRaw, &m); err != nil {
		return Entry{}, err
	}
	abs, _ := os.Getwd()
	if strings.HasPrefix(path, "/") {
		abs = ""
	}
	url := "file://" + strings.TrimSuffix(abs, "/") + "/" + path
	if abs == "" {
		url = "file://" + path
	}
	return Entry{ID: m.ID, Version: m.AppVersion, Description: m.Description, BundleURL: url, BundleSHA256: hex.EncodeToString(sum[:])}, nil
}

// BundleFacts are the runtime facts about a built bundle that the publisher
// needs to fill metadata.json + the v2 catalogue entry.
type BundleFacts struct {
	ID             string
	Version        string
	Description    string
	Publisher      string // store.publisher from the (signed) manifest
	SHA256         string // sha256 of the tarball
	BundleBytes    int64  // tarball size
	InstalledBytes int64  // size of the binary inside the bundle
}

// ReadBundleFacts opens a local bundle tarball and extracts the facts above.
func ReadBundleFacts(path string) (BundleFacts, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BundleFacts{}, err
	}
	sum := sha256.Sum256(raw)
	mfRaw, binBytes, err := extractBundle(raw)
	if err != nil {
		return BundleFacts{}, err
	}
	var m struct {
		ID          string `json:"id"`
		AppVersion  string `json:"app_version"`
		Description string `json:"description"`
		Store       struct {
			Publisher string `json:"publisher"`
		} `json:"store"`
	}
	if err := json.Unmarshal(mfRaw, &m); err != nil {
		return BundleFacts{}, err
	}
	return BundleFacts{
		ID:             m.ID,
		Version:        m.AppVersion,
		Description:    m.Description,
		Publisher:      m.Store.Publisher,
		SHA256:         hex.EncodeToString(sum[:]),
		BundleBytes:    int64(len(raw)),
		InstalledBytes: int64(len(binBytes)),
	}, nil
}

// VerifyCatalogue verifies every entry in a catalogue.json file. If oldPath is
// non-empty, entries are diffed against it for the downgrade check.
func VerifyCatalogue(newPath, oldPath string) ([]Result, error) {
	cat, err := loadCatalogue(newPath)
	if err != nil {
		return nil, err
	}
	var prev map[string]Entry
	if oldPath != "" {
		if old, err := loadCatalogue(oldPath); err == nil {
			prev = map[string]Entry{}
			for _, e := range old.Apps {
				prev[e.ID] = e
			}
		}
	}
	out := make([]Result, 0, len(cat.Apps))
	for _, e := range cat.Apps {
		var p *Entry
		if prev != nil {
			if pe, ok := prev[e.ID]; ok {
				p = &pe
			}
		}
		out = append(out, VerifyEntry(e, p))
	}
	return out, nil
}

func loadCatalogue(p string) (*Catalogue, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Catalogue
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

// fetch reads bundle bytes from an http(s):// or file:// URL.
func fetch(url string) ([]byte, error) {
	if strings.HasPrefix(url, "file://") {
		return os.ReadFile(strings.TrimPrefix(url, "file://"))
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// extractBundle reads a .tar.gz and returns (manifest.json bytes, binary bytes).
// The binary is located via manifest.binary.path.
func extractBundle(targz []byte) (mfRaw, binBytes []byte, err error) {
	gz, err := gzip.NewReader(strings.NewReader(string(targz)))
	if err != nil {
		return nil, nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 256<<20))
		if err != nil {
			return nil, nil, err
		}
		files[path.Clean(strings.TrimPrefix(hdr.Name, "./"))] = b
	}
	mfRaw, ok := files["manifest.json"]
	if !ok {
		return nil, nil, fmt.Errorf("no manifest.json in bundle")
	}
	var m struct {
		Binary struct {
			Path string `json:"path"`
		} `json:"binary"`
	}
	if err := json.Unmarshal(mfRaw, &m); err != nil {
		return nil, nil, fmt.Errorf("manifest binary.path: %w", err)
	}
	binBytes, ok = files[path.Clean(m.Binary.Path)]
	if !ok {
		return nil, nil, fmt.Errorf("binary %q named in manifest not found in bundle", m.Binary.Path)
	}
	return mfRaw, binBytes, nil
}

func hasHelp(exposes []string) bool {
	for _, e := range exposes {
		if strings.HasSuffix(e, ".help") {
			return true
		}
	}
	return false
}

func short(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// compareSemver returns -1/0/1 comparing MAJOR.MINOR.PATCH (prerelease ignored
// beyond presence). Good enough for the downgrade guard; the supervisor has the
// authoritative comparator.
func compareSemver(a, b string) int {
	an := strings.SplitN(a, "-", 2)[0]
	bn := strings.SplitN(b, "-", 2)[0]
	ap := strings.Split(an, ".")
	bp := strings.Split(bn, ".")
	for i := 0; i < 3; i++ {
		av, bv := atoi(get(ap, i)), atoi(get(bp, i))
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func get(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "0"
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
