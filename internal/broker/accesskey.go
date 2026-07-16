package broker

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// Access keys are the broker's front door.
//
// A signed pilot identity (identity.go) proves the caller holds the private key
// for a public key, but the caller generates that keypair itself. It is a
// pseudonym: it attributes usage, it does not authorize it.
//
// The access key is the other half. Identity says WHO you are; the access key
// says you are ALLOWED TO BE HERE. Both are required, and neither substitutes
// for the other: a shared key cannot attribute usage to a user, and a
// self-generated identity cannot establish that a caller is a pilot client at
// all. Any app that grants something of value on first contact needs both.
//
// Keys are compared as SHA-256 digests in constant time, and only the digests
// are held in memory, so the broker never keeps a usable secret it could leak
// through a crash dump or a debug endpoint.
type AccessKeys struct {
	digests map[string]string // hex(sha256(key)) -> label, for revocation + attribution
}

// AccessKeyHeader is the dedicated header. Authorization: Bearer <key> is also
// accepted so ordinary HTTP clients work without a custom header.
const AccessKeyHeader = "X-Pilot-Access-Key"

// NewAccessKeys builds the key set from "label:key" (or bare "key") entries.
// Blank entries are ignored so a trailing comma in config is not a silent
// "no keys configured" — which would otherwise disable the gate.
func NewAccessKeys(entries []string) *AccessKeys {
	ak := &AccessKeys{digests: map[string]string{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		label, key := "", e
		if i := strings.Index(e, ":"); i > 0 {
			label, key = e[:i], e[i+1:]
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		sum := sha256.Sum256([]byte(key))
		ak.digests[hex.EncodeToString(sum[:])] = label
	}
	return ak
}

// Len reports how many keys are configured (diagnostics; never logs the keys).
func (a *AccessKeys) Len() int {
	if a == nil {
		return 0
	}
	return len(a.digests)
}

// presented pulls the candidate key off a request.
func presented(h func(string) string) string {
	if v := strings.TrimSpace(h(AccessKeyHeader)); v != "" {
		return v
	}
	// Authorization: Bearer <key>
	if v := strings.TrimSpace(h("Authorization")); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			return strings.TrimSpace(v[7:])
		}
	}
	return ""
}

// Check verifies the presented key and returns its label.
//
// It FAILS CLOSED in the case that matters most: an AccessKeys with no keys
// configured authorizes NOTHING. A missing or blank BROKER_ACCESS_KEYS must
// never degrade to "open to everyone" — an absent control has to read as "deny",
// not as "allow". main refuses to boot in that state rather than serve.
func (a *AccessKeys) Check(h func(string) string) (label string, ok bool) {
	if a == nil || len(a.digests) == 0 {
		return "", false
	}
	key := presented(h)
	if key == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	want := hex.EncodeToString(sum[:])
	// Constant-time scan over every digest: comparing hex digests of a hash means
	// a timing signal cannot reveal the key, and not short-circuiting keeps the
	// work independent of which key matched.
	found, lbl := 0, ""
	for d, l := range a.digests {
		if subtle.ConstantTimeCompare([]byte(d), []byte(want)) == 1 {
			found, lbl = 1, l
		}
	}
	return lbl, found == 1
}

// requireAccessKey enforces the gate for an app that demands one. It answers 401
// with a WWW-Authenticate hint and no detail about why: a caller learns only
// that a valid key is required, never whether a key exists or was close.
func (b *Broker) requireAccessKey(w http.ResponseWriter, r *http.Request, app *AppEntry) bool {
	if !app.RequireAccessKey {
		return true
	}
	if _, ok := b.AccessKeys.Check(r.Header.Get); ok {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="pilot-broker"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "this app requires a pilot access key — upgrade your pilot client (`pilotctl appstore upgrade`) or see https://pilotprotocol.network/docs/access-keys",
	})
	return false
}
