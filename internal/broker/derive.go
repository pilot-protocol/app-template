package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// Per-user rotation: the derived key binds a per-user rotation counter (rot) in
// addition to the version + caller id. Bumping a single user's rot (smol.rotate)
// invalidates their old key WITHOUT touching anyone else and WITHOUT resetting
// their credit (credit is keyed by the caller, not the key) — this is how a
// leaked key is revoked.

// Derived per-user cloud keys — the broker "piecemeals" the one master account
// into a distinct, unforgeable key per Pilot user WITHOUT storing a secret per
// user. The key is a MAC over the caller's VERIFIED identity, so:
//
//   - it is bound to exactly one Pilot pubkey (the callerID is embedded and
//     sealed), so it "only works per user" — it cannot be re-pointed at another
//     user without the broker secret, which never leaves the broker;
//   - the broker re-derives it from the identity that identity.go:Verify just
//     proved on the request, so a client can never present a key for a pubkey it
//     does not control;
//   - no per-user secret is persisted — mint and validate are pure functions of
//     (secret, version, callerID).
//
// Token wire format (opaque to the user, stored as their "proprietary api key"):
//
//	sk-smol-<version>.<base64url(callerID)>.<base64url(HMAC-SHA256(secret, version‖callerID))>
//
// <version> is a rotation tag: the broker mints with its current version and can
// validate against a map of {version: secret} during a rotation grace window.
const derivedPrefix = "sk-smol-"

// b64 is the delimiter-safe (no '+' '/' '=') encoding used for both segments so
// the '.'-separated token is unambiguous.
var b64 = base64.RawURLEncoding

// macMessage is the exact byte string the MAC covers: the version byte, the
// per-user rotation counter, and the canonical callerID bytes. Binding the
// version prevents a token minted under one secret from validating under another
// (global rotation safety); binding rot lets a single user revoke their key.
func macMessage(version byte, rot int, caller CallerID) []byte {
	rotStr := strconv.Itoa(rot)
	msg := make([]byte, 0, 1+len(rotStr)+1+len(caller))
	msg = append(msg, version)
	msg = append(msg, rotStr...)
	msg = append(msg, '\n')
	msg = append(msg, caller...)
	return msg
}

func computeMAC(secret []byte, version byte, rot int, caller CallerID) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(macMessage(version, rot, caller))
	return m.Sum(nil)
}

// DeriveKey mints the per-user token for a verified caller at rotation rot.
// secret must be the broker's derive secret for the given version.
func DeriveKey(secret []byte, version byte, rot int, caller CallerID) string {
	mac := computeMAC(secret, version, rot, caller)
	var sb strings.Builder
	sb.WriteString(derivedPrefix)
	sb.WriteString(strconv.Itoa(int(version)))
	sb.WriteByte('.')
	sb.WriteString(b64.EncodeToString([]byte(caller)))
	sb.WriteByte('.')
	sb.WriteString(b64.EncodeToString(mac))
	return sb.String()
}

// ParseDerived splits a token into its parts WITHOUT verifying the MAC. ok is
// false on any structural malformation. The returned caller is the decoded
// callerID; it is NOT trusted until ValidateDerived confirms the MAC.
func ParseDerived(token string) (version byte, caller string, mac []byte, ok bool) {
	rest, found := strings.CutPrefix(token, derivedPrefix)
	if !found {
		return 0, "", nil, false
	}
	vStr, tail, found := strings.Cut(rest, ".")
	if !found {
		return 0, "", nil, false
	}
	callerB64, macB64, found := strings.Cut(tail, ".")
	if !found {
		return 0, "", nil, false
	}
	v, err := strconv.Atoi(vStr)
	if err != nil || v < 0 || v > 255 {
		return 0, "", nil, false
	}
	callerBytes, err := b64.DecodeString(callerB64)
	if err != nil {
		return 0, "", nil, false
	}
	macBytes, err := b64.DecodeString(macB64)
	if err != nil || len(macBytes) != sha256.Size {
		return 0, "", nil, false
	}
	return byte(v), string(callerBytes), macBytes, true
}

// ValidateDerived checks a token's MAC against the secret for its version AND the
// given rotation counter, and on success returns the embedded (now-trusted)
// callerID. The caller supplies rot — the broker looks up the user's CURRENT rot
// (by the token's callerID) so an old (pre-rotation) key fails. secretsByVersion
// holds every currently-accepted {version: secret} (one normally; two during a
// global-secret grace window). Comparison is constant-time.
func ValidateDerived(secretsByVersion map[byte][]byte, token string, rot int) (caller string, ok bool) {
	version, callerStr, mac, parsed := ParseDerived(token)
	if !parsed {
		return "", false
	}
	secret, known := secretsByVersion[version]
	if !known || len(secret) == 0 {
		return "", false
	}
	want := computeMAC(secret, version, rot, CallerID(callerStr))
	if !hmac.Equal(mac, want) {
		return "", false
	}
	return callerStr, true
}
