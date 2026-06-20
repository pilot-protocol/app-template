// Package broker is the Pilot managed-key broker: one service that holds each
// app's master key, verifies WHO is calling (a signed Pilot identity), meters
// per caller, and forwards to the partner API. This file is the security core —
// verifying the caller identity so metering/billing can't be spoofed.
package broker

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"time"
)

// Request headers a caller (the Pilot daemon, on behalf of an agent) attaches.
// The daemon signs each call with its ed25519 identity key; the broker verifies.
const (
	HdrCaller    = "X-Pilot-Caller"    // base64(ed25519 public key) — the identity
	HdrTimestamp = "X-Pilot-Timestamp" // unix seconds — bounds replay
	HdrSignature = "X-Pilot-Signature" // base64(ed25519 signature) over the canonical request
)

var (
	ErrMissingIdentity = errors.New("broker: missing caller identity headers")
	ErrBadPubkey       = errors.New("broker: invalid caller public key")
	ErrStale           = errors.New("broker: request timestamp outside the allowed window")
	ErrBadSignature    = errors.New("broker: caller signature does not verify")
)

// VerifyConfig controls caller-identity verification.
type VerifyConfig struct {
	Window time.Duration    // max clock skew / replay window; default 5m
	Now    func() time.Time // injectable clock (tests); default time.Now
}

// CallerID is a verified caller identity (its ed25519 public key, base64).
type CallerID string

// canonical is the exact byte string a caller signs for a request. Binding the
// method, path, timestamp, and a hash of the body means a captured signature
// can't be replayed against a different call or a tampered body.
func canonical(method, path, ts string, body []byte) []byte {
	sum := sha256.Sum256(body)
	out := make([]byte, 0, len(method)+len(path)+len(ts)+48)
	out = append(out, method...)
	out = append(out, '\n')
	out = append(out, path...)
	out = append(out, '\n')
	out = append(out, ts...)
	out = append(out, '\n')
	out = append(out, []byte(base64.RawStdEncoding.EncodeToString(sum[:]))...)
	return out
}

// decodeB64 accepts raw- or std-base64 (whatever the caller emitted).
func decodeB64(s string) ([]byte, bool) {
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	return nil, false
}

func encodeKey(pub ed25519.PublicKey) string { return base64.RawStdEncoding.EncodeToString(pub) }
func encodeSig(sig []byte) string            { return base64.RawStdEncoding.EncodeToString(sig) }

// Verify checks the signed-identity headers for a request and, on success,
// returns the verified caller id (the signer's pubkey). It rejects missing,
// malformed, stale (replayed), tampered, or forged requests.
func (c VerifyConfig) Verify(h func(string) string, method, path string, body []byte) (CallerID, error) {
	now := c.Now
	if now == nil {
		now = time.Now
	}
	win := c.Window
	if win <= 0 {
		win = 5 * time.Minute
	}

	pkB64, ts, sigB64 := h(HdrCaller), h(HdrTimestamp), h(HdrSignature)
	if pkB64 == "" || ts == "" || sigB64 == "" {
		return "", ErrMissingIdentity
	}

	pk, ok := decodeB64(pkB64)
	if !ok || len(pk) != ed25519.PublicKeySize {
		return "", ErrBadPubkey
	}

	tsi, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrStale
	}
	skew := now().Unix() - tsi
	if skew < 0 {
		skew = -skew
	}
	if time.Duration(skew)*time.Second > win {
		return "", ErrStale
	}

	sig, ok := decodeB64(sigB64)
	if !ok || len(sig) != ed25519.SignatureSize {
		return "", ErrBadSignature
	}
	if !ed25519.Verify(ed25519.PublicKey(pk), canonical(method, path, ts, body), sig) {
		return "", ErrBadSignature
	}
	// constant-time confirm the returned id matches the header we verified.
	if subtle.ConstantTimeCompare([]byte(pkB64), []byte(encodeKey(pk))) != 1 {
		return CallerID(encodeKey(pk)), nil // normalize to canonical encoding
	}
	return CallerID(pkB64), nil
}

// Sign builds the headers a caller attaches. It is the reference the Pilot
// daemon (and the tests) use to sign a request with its identity key.
func Sign(priv ed25519.PrivateKey, method, path string, body []byte, now time.Time) map[string]string {
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := ed25519.Sign(priv, canonical(method, path, ts, body))
	return map[string]string{
		HdrCaller:    encodeKey(priv.Public().(ed25519.PublicKey)),
		HdrTimestamp: ts,
		HdrSignature: encodeSig(sig),
	}
}
