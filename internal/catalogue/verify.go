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
	"sort"
	"strings"
	"time"

	"github.com/pilot-protocol/app-store/pkg/manifest"
)

// Bundle-extraction caps. A real adapter bundle is a few files totaling tens of
// MB; these bounds protect the review gate from a crafted/zip-bomb tarball.
const (
	maxBundleEntries    = 64
	maxBundleFileBytes  = 256 << 20 // 256 MiB per file
	maxBundleTotalBytes = 512 << 20 // 512 MiB total decompressed
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

	// RenamedTo and Hidden mark a tombstone: an id kept only so already-installed
	// copies keep their publisher pin and so the old id still redirects. A
	// tombstone ships no bundle, so the bundle checks do not apply to it.
	RenamedTo string `json:"renamed_to,omitempty"`
	Hidden    bool   `json:"hidden,omitempty"`

	// Bundles is the optional v2 per-platform map, keyed "os/arch". pilotctl
	// installs from this when present, so the review gate has to verify every
	// variant — not just the legacy single BundleURL above.
	Bundles map[string]BundleVariant `json:"bundles,omitempty"`
}

// BundleVariant is one platform's tarball and its pinned digest.
type BundleVariant struct {
	BundleURL    string `json:"bundle_url"`
	BundleSHA256 string `json:"bundle_sha256"`
}

// isTombstone reports whether the entry is a retired id kept only for pin
// continuity and redirection, rather than an installable app.
func (e Entry) isTombstone() bool {
	return e.RenamedTo != "" || (e.Hidden && e.BundleURL == "")
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
	return verifyEntry(e, prev, false)
}

// verifyEntry runs the gate. inCatalogue is true only for entries read out of a
// catalogue.json: a published entry is expected to declare its platforms, while
// a local bundle passed to `pilot-app verify <bundle.tar.gz>` legitimately has
// no platform context to check against.
func verifyEntry(e Entry, prev *Entry, inCatalogue bool) Result {
	r := Result{ID: e.ID}

	// A tombstone carries no bundle by design; verifying one only produces noise.
	if e.isTombstone() {
		r.pass("tombstone entry", "renamed to "+e.RenamedTo+"; no bundle to verify")
		return r
	}

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

	// The legacy single bundle carries no platform declaration, so we can only
	// report what it is; the per-platform variants below are the checkable part.
	if len(e.Bundles) > 0 {
		verifyVariants(&r, e)
	} else if inCatalogue {
		r.check("declares per-platform bundles", false, "",
			"no bundles map: every host installs the single bundle_url, which holds a "+execFormat(binBytes)+
				" binary — hosts on any other OS install something they cannot execute, and the app silently never spawns")
	}

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

// verifyVariants fetches every declared platform bundle and checks its digest
// and its binary format. Without this the review gate only ever saw the legacy
// bundle_url, while pilotctl installs from the per-platform map.
func verifyVariants(r *Result, e Entry) {
	plats := make([]string, 0, len(e.Bundles))
	for p := range e.Bundles {
		plats = append(plats, p)
	}
	sort.Strings(plats)

	for _, plat := range plats {
		v := e.Bundles[plat]
		if v.BundleURL == "" {
			r.fail("bundle for "+plat, "empty bundle_url")
			continue
		}
		raw, err := fetch(v.BundleURL)
		if !r.check("bundle for "+plat+" resolves", err == nil, v.BundleURL, fmt.Sprintf("fetch %s: %v", v.BundleURL, err)) {
			continue
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if !r.check("bundle_sha256 for "+plat, strings.EqualFold(got, v.BundleSHA256),
			got, fmt.Sprintf("got %s, catalogue says %s", got, v.BundleSHA256)) {
			continue
		}
		mfRaw, binBytes, err := extractBundle(raw)
		if !r.check("bundle for "+plat+" contains manifest.json + bin/<binary>", err == nil, "", fmt.Sprintf("%v", err)) {
			continue
		}

		// The per-platform bundle is the one pilotctl actually installs, so it
		// gets the same manifest scrutiny as the legacy single bundle — not
		// just a digest and a format sniff.
		m, err := manifest.Parse(mfRaw)
		if !r.check("manifest parses for "+plat, err == nil, "", fmt.Sprintf("%v", err)) {
			continue
		}
		binSum := sha256.Sum256(binBytes)
		binSHA := hex.EncodeToString(binSum[:])
		r.check("binary.sha256 pin for "+plat, strings.EqualFold(binSHA, m.Binary.SHA256),
			binSHA, fmt.Sprintf("manifest pins %s, binary is %s", m.Binary.SHA256, binSHA))
		r.check("signature verifies for "+plat, m.VerifySignature() == nil,
			"signed by "+short(m.Store.Publisher), errString(m.VerifySignature()))
		r.check("id/version match for "+plat, e.ID == m.ID && e.Version == m.AppVersion,
			e.ID+" "+e.Version,
			fmt.Sprintf("catalogue %s %s != manifest %s %s", e.ID, e.Version, m.ID, m.AppVersion))

		r.checkPlatformBinary(plat, binBytes)
	}
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
		out = append(out, verifyEntry(e, p, true))
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
	var total int64
	entries := 0
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
		// Bound decompression: a real adapter bundle is a handful of files
		// (manifest.json, bin/<binary>, optional install.json/install.sh). Cap
		// entry count and total decompressed size so a crafted/zip-bomb bundle
		// can't exhaust the review gate's memory.
		entries++
		if entries > maxBundleEntries {
			return nil, nil, fmt.Errorf("bundle has too many files (> %d)", maxBundleEntries)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxBundleFileBytes))
		if err != nil {
			return nil, nil, err
		}
		total += int64(len(b))
		if total > maxBundleTotalBytes {
			return nil, nil, fmt.Errorf("bundle decompressed size exceeds %d bytes", maxBundleTotalBytes)
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

// execFormat names the executable container a binary is in, from its magic
// bytes. Enough to tell a Linux build from a macOS one, which is the mistake
// this gate exists to catch.
func execFormat(b []byte) string {
	if len(b) >= 4 {
		switch {
		case b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F':
			return "elf"
		// Mach-O, 32/64-bit, both byte orders, plus the fat/universal wrappers.
		case (b[0] == 0xcf || b[0] == 0xce) && b[1] == 0xfa && b[2] == 0xed && b[3] == 0xfe,
			b[0] == 0xfe && b[1] == 0xed && b[2] == 0xfa && (b[3] == 0xce || b[3] == 0xcf),
			// 0xcafebabe is also the Java class magic; harmless here because the
			// verdict is only used to compare against a declared native platform.
			b[0] == 0xca && b[1] == 0xfe && b[2] == 0xba && b[3] == 0xbe,
			b[0] == 0xbe && b[1] == 0xba && b[2] == 0xfe && b[3] == 0xca:
			return "macho"
		case b[0] == 'M' && b[1] == 'Z':
			return "pe"
		case b[0] == '#' && b[1] == '!':
			return "script"
		}
	}
	return "unknown"
}

// formatForOS is the executable container a given GOOS must ship.
func formatForOS(goos string) string {
	switch goos {
	case "linux":
		return "elf"
	case "darwin":
		return "macho"
	case "windows":
		return "pe"
	}
	return ""
}

// checkPlatformBinary asserts that the binary inside a platform's bundle is
// actually executable on that platform. A publisher building on a Mac ships a
// Mach-O binary; without this check the sha matches on every host, install
// succeeds, and the app simply never spawns on Linux with no error anywhere.
func (r *Result) checkPlatformBinary(plat string, binBytes []byte) {
	goos, _, ok := strings.Cut(plat, "/")
	if !ok {
		return
	}
	want := formatForOS(goos)
	if want == "" {
		return
	}
	got := execFormat(binBytes)
	if got == "script" {
		// A shebang script is portable by itself but depends on an interpreter
		// the bundle does not carry; flag it rather than silently passing.
		r.fail("binary matches "+plat, "bundle ships a #! script, not a native "+want+" binary; the interpreter is not part of the bundle")
		return
	}
	r.check("binary matches "+plat, got == want,
		got+" as expected for "+goos,
		fmt.Sprintf("bundle for %s contains a %s binary, want %s — it will install and then never spawn", plat, got, want))
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

// compareSemver returns -1/0/1 comparing MAJOR.MINOR.PATCH plus prerelease
// precedence per semver §11: at equal numeric versions, a version WITH a
// prerelease ranks below one without (1.0.0-rc1 < 1.0.0), so the downgrade
// guard rejects republishing a prerelease over a released build.
func compareSemver(a, b string) int {
	aParts := strings.SplitN(a, "-", 2)
	bParts := strings.SplitN(b, "-", 2)
	ap := strings.Split(aParts[0], ".")
	bp := strings.Split(bParts[0], ".")
	for i := 0; i < 3; i++ {
		av, bv := atoi(get(ap, i)), atoi(get(bp, i))
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	// Numeric versions equal — apply prerelease precedence.
	aPre, bPre := len(aParts) == 2, len(bParts) == 2
	switch {
	case !aPre && !bPre:
		return 0
	case aPre && !bPre:
		return -1 // a is a prerelease of the same version → lower
	case !aPre && bPre:
		return 1
	default:
		return comparePrerelease(aParts[1], bParts[1])
	}
}

// comparePrerelease compares two prerelease strings per semver §11: identifiers
// are compared dot-by-dot; numeric identifiers compare numerically and rank
// below alphanumeric ones; a smaller set of identifiers ranks lower when all
// preceding identifiers are equal.
func comparePrerelease(a, b string) int {
	ai := strings.Split(a, ".")
	bi := strings.Split(b, ".")
	for i := 0; i < len(ai) || i < len(bi); i++ {
		if i >= len(ai) {
			return -1
		}
		if i >= len(bi) {
			return 1
		}
		x, y := ai[i], bi[i]
		xn, xNum := numericIdent(x)
		yn, yNum := numericIdent(y)
		switch {
		case xNum && yNum:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xNum && !yNum:
			return -1 // numeric ranks below alphanumeric
		case !xNum && yNum:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

// numericIdent reports whether s is an all-digit identifier and its value.
func numericIdent(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
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
