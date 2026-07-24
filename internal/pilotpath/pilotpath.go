// Package pilotpath holds route literals shared between the scaffold generator
// (internal/scaffold) and the broker server (internal/broker) — two
// independently-compiled packages that must agree on the same wire path
// without importing one another. Defining the literal once here, and having
// both sides reference it, means the two call sites cannot drift apart the
// way two independently-maintained "/_pilot/balance" string literals could.
package pilotpath

// Balance is the broker's reserved, per-user credit-balance route for managed
// (credit-metered, non-provisioned) apps. A GET to this path returns the
// caller's remaining budget straight from the broker's own credit ledger — no
// partner API call, no debit, never a 402.
//
//   - scaffold.BalanceMetaPath is the path scaffold wires the generated
//     `<ns>.balance` method to (internal/scaffold/config.go).
//   - broker's pilotBalancePath is the path the broker answers from its ledger
//     (internal/broker/broker.go).
//
// Both are defined as this constant, not a re-typed literal, so they can never
// silently diverge.
const Balance = "/_pilot/balance"
