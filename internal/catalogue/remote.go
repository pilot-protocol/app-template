package catalogue

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// DefaultCatalogueURL is the live, signed catalogue index on the platform repo's
// main branch. The update gate reads it to learn the current owner (publisher
// pin) and version of an app id. Override with PILOT_CATALOGUE_URL (the .sig is
// the same URL + ".sig").
const DefaultCatalogueURL = "https://raw.githubusercontent.com/pilot-protocol/pilotprotocol/main/catalogue/catalogue.json"

// catalogueTrustPubKeyB64 is the std-base64 ed25519 public key the catalogue is
// signed with (mirrors pilotprotocol/internal/catalogtrust). Override at runtime
// with PILOT_CATALOGUE_PUBKEY for a rotated key or a test fixture. Empty (env set
// to "-") disables signature verification (local testing only).
const catalogueTrustPubKeyB64 = "iHdBWayA/hYjkwUOZopTXY70qOlR90d6ii/hin0ZMdI="

// Owner is the trust-relevant slice of a catalogue entry: who may publish updates
// for the id (Publisher pin) and the current published Version.
type Owner struct {
	Version   string
	Publisher string // "ed25519:<base64>" — the key that owns this app's updates
}

// liveEntry is the catalogue entry shape we need from the live index (a superset
// of the gate's Entry — it also carries the publisher pin).
type liveEntry struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Publisher string `json:"publisher"`
}

type liveCatalogue struct {
	Version int         `json:"version"`
	Apps    []liveEntry `json:"apps"`
}

// CatalogueURL returns the live catalogue URL (env override or default).
func CatalogueURL() string {
	if v := strings.TrimSpace(os.Getenv("PILOT_CATALOGUE_URL")); v != "" {
		return v
	}
	return DefaultCatalogueURL
}

// FetchOwners downloads the live catalogue + its detached signature, verifies the
// signature against the trust key, and returns id -> Owner. A verified fetch is
// the basis of the update gate: it is the authoritative record of who owns each
// app id. Network/parse/signature failures are returned as errors (fail-closed —
// callers must not silently treat a fetch failure as "no owner").
func FetchOwners(catURL string) (map[string]Owner, error) {
	raw, err := fetch(catURL)
	if err != nil {
		return nil, fmt.Errorf("fetch catalogue %s: %w", catURL, err)
	}
	if err := verifyCatalogueSig(catURL, raw); err != nil {
		return nil, err
	}
	var c liveCatalogue
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse catalogue: %w", err)
	}
	owners := make(map[string]Owner, len(c.Apps))
	for _, e := range c.Apps {
		owners[e.ID] = Owner{Version: e.Version, Publisher: e.Publisher}
	}
	return owners, nil
}

// CheckUpdate applies the update ownership + monotonicity gate for one app
// against the live owners map:
//
//   - unknown id        → first publish (trust-on-first-use); no constraint.
//   - known id          → the new version MUST NOT be a downgrade (a re-publish of
//     the same version is allowed — idempotent), AND, when the
//     new bundle's signer is known, it MUST equal the owning
//     publisher pin (you can only update an app you own).
//
// newPublisher is the "ed25519:<base64>" that signed the new bundle, or "" when
// it is not yet known (the rich form path signs server-side at build time — the
// publisher check then runs there). It returns a Result of named pass/fail checks
// so callers can print them like the rest of the gate.
func CheckUpdate(owners map[string]Owner, id, newVersion, newPublisher string) Result {
	r := Result{ID: id}
	owner, exists := owners[id]
	if !exists {
		r.pass("app ownership", "new app id — first publish (no prior owner)")
		return r
	}
	r.check("version is not a downgrade", compareSemver(newVersion, owner.Version) >= 0,
		fmt.Sprintf("%s ≥ %s", newVersion, owner.Version),
		fmt.Sprintf("%s is older than the published %s — an update must not move the version backwards", newVersion, owner.Version))

	switch {
	case owner.Publisher == "":
		r.fail("owned by a pinned publisher", "the published entry has no publisher pin — refuse to update an unpinned app")
	case newPublisher == "":
		r.pass("signed by the owning publisher", "publisher checked server-side at build/sign time (rich form path)")
	default:
		r.check("signed by the owning publisher", newPublisher == owner.Publisher,
			"matches the owning key "+short(owner.Publisher),
			fmt.Sprintf("%s is owned by %s; this bundle is signed by %s — updates must be signed by the original publisher key", id, short(owner.Publisher), short(newPublisher)))
	}
	return r
}

// verifyCatalogueSig fetches catURL+".sig" and verifies the base64 ed25519
// signature over the exact catalogue bytes. Honors PILOT_CATALOGUE_PUBKEY; the
// sentinel "-" disables verification (local only).
func verifyCatalogueSig(catURL string, body []byte) error {
	pubB64 := catalogueTrustPubKeyB64
	if v := strings.TrimSpace(os.Getenv("PILOT_CATALOGUE_PUBKEY")); v != "" {
		pubB64 = v
	}
	if pubB64 == "-" {
		return nil // explicitly disabled (tests)
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("bad catalogue trust pubkey")
	}
	sigRaw, err := fetch(catURL + ".sig")
	if err != nil {
		return fmt.Errorf("fetch catalogue signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
	if err != nil {
		return fmt.Errorf("decode catalogue signature: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), body, sig) {
		return fmt.Errorf("catalogue signature does not verify against the trust key")
	}
	return nil
}
