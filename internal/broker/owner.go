package broker

import (
	"crypto/subtle"
	"errors"
	"time"
)

// constantTimeEqual compares two identity strings without leaking their contents
// through timing. Owner ids are public keys, but comparing them in constant time
// costs nothing and keeps the authorization predicate free of an oracle.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// OwnerStore is the broker-side resource-ownership ledger: it records WHICH
// caller owns WHICH upstream resource id, for apps whose partner account cannot
// be split per user.
//
// Why this exists: some managed apps are one shared partner account that cannot
// be split per user (e.g. an account bound to a provider campaign). Upstream,
// every pilot user is the same customer, so the partner cannot tell which user a
// resource belongs to and cannot enforce isolation. The broker therefore has to:
// every resource is claimed by its creator, and anything not provably owned by
// the caller is invisible to them.
//
// Deny-by-default is the whole point: a resource with NO ledger row is owned by
// NOBODY and is refused to EVERYONE (including pre-existing resources created
// before this ledger shipped). That is deliberate — it is what makes the legacy
// shared test number unreachable rather than up for grabs.
type OwnerStore interface {
	// Claim records caller as the owner of (app, rtype, rid). It is idempotent
	// and FIRST-WRITER-WINS: if a different caller already owns the resource the
	// existing owner is kept and ErrOwned is returned. It must never silently
	// transfer ownership — that would be a takeover primitive.
	Claim(app, rtype, rid, caller string, now time.Time) error
	// OwnerOf returns the recorded owner of a resource, if any.
	OwnerOf(app, rtype, rid string) (caller string, found bool, err error)
	// OwnedSet returns the set of resource ids of rtype owned by caller. Used to
	// filter list responses down to the caller's own resources.
	OwnedSet(app, rtype, caller string) (map[string]bool, error)
	// Release drops a resource row (e.g. a number was released upstream) so the
	// id can be re-claimed if the partner recycles it.
	Release(app, rtype, rid string) error
}

// ErrOwned is returned by Claim when the resource already belongs to a different
// caller. It signals an attempted takeover, not a routine race.
var ErrOwned = errors.New("broker: resource already owned by another caller")

// Owns reports whether caller owns (app, rtype, rid). It is the single
// authorization predicate for tenancy and FAILS CLOSED: any store error, or an
// unknown/unclaimed resource, returns false. Callers must treat false as "deny".
func Owns(s OwnerStore, app, rtype, rid, caller string) bool {
	if s == nil || rid == "" || caller == "" {
		return false
	}
	owner, found, err := s.OwnerOf(app, rtype, rid)
	if err != nil || !found {
		return false
	}
	return constantTimeEqual(owner, caller)
}

// ownerStore returns the tenancy ledger if the configured Store implements it.
//
// It returns nil when the store cannot record ownership. Every tenancy check
// treats a nil ledger as "deny" (see Owns / EnforceRequest), so an app that
// declares tenancy against a store that cannot enforce it fails CLOSED — it
// refuses traffic instead of silently serving it unisolated.
func (b *Broker) ownerStore() OwnerStore {
	os, ok := b.Store.(OwnerStore)
	if !ok {
		return nil
	}
	return os
}
