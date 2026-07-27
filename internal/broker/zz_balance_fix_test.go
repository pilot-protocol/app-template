package broker

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/pilot-protocol/app-template/internal/pilotpath"
)

// creditBrokerCooldown is creditBroker's twin, but with a non-zero
// mint_cooldown_ms — the exact config shape that used to make a repeat
// `.balance` read 429 with "re-provision cooldown" instead of returning the
// balance (finding #2).
func creditBrokerCooldown(t *testing.T, upStatus int, cooldownMs int, now time.Time) (*Broker, *int) {
	t.Helper()
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(upStatus)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.test","upstream":"`+up.URL+`","key_env":"TEST_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer",
		"allow":["/echo"],
		"credit":{"seed_credits":100,"default_cost":10,"mint_cooldown_ms":`+strconv.Itoa(cooldownMs)+`}
	}]`), func(string) string { return "MASTERKEY" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Now: fixedClock(now)}
	return b, &hits
}

// TestBalanceMeta_ReadIgnoresMintCooldown pins the fix for finding #2: a credit
// app configured with mint_cooldown_ms > 0 (to slow rapid re-provision churn on
// real mints) must NOT have that cooldown leak onto the free `.balance` read.
// Before the fix, serveCreditBalance called seedCaller -> ProvisionStore.Provision
// on every hit — including repeats — and MemStore.Provision's cooldown check
// fires on every repeat touch, so a second balance call inside the cooldown
// window 429'd with "re-provision cooldown" instead of returning the balance.
func TestBalanceMeta_ReadIgnoresMintCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// A full minute of cooldown — comfortably longer than the fixed test clock
	// ever advances, so ANY repeat call within it would have 429'd pre-fix.
	b, _ := creditBrokerCooldown(t, 200, 60_000, now)
	_, priv := newKey(t)

	// First-ever call seeds the caller; this is the caller's genuine first
	// touch, so it can never itself hit the cooldown (nothing to be too soon
	// after).
	bal, rec := balanceOf(t, b, priv, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("first balance call: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if bal != 100 {
		t.Fatalf("seeded balance = %d, want 100", bal)
	}

	// Repeat calls, still well inside the 60s cooldown window (fixed clock:
	// zero time has elapsed at all) — every one of these must return the
	// balance, never the mint-cooldown 429.
	for i := 0; i < 5; i++ {
		bal, rec = balanceOf(t, b, priv, now)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("repeat balance call %d: got 429 %s (mint cooldown leaked onto a read)", i, rec.Body.String())
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("repeat balance call %d: status %d, want 200 (body %s)", i, rec.Code, rec.Body.String())
		}
		if bal != 100 {
			t.Fatalf("repeat balance call %d = %d, want 100 (unspent)", i, bal)
		}
	}

	// A real metered call is a mint touch (seedCaller -> ProvisionStore.Provision)
	// and so is legitimately still subject to the cooldown by elapsed wall time —
	// this test only asserts the READ path is exempt, not that cooldown
	// enforcement disappeared everywhere. Advance past the cooldown window so the
	// metered call itself isn't the thing under test here.
	later := now.Add(61 * time.Second)
	b.Verify = VerifyConfig{Now: fixedClock(later)}
	spend := httptest.NewRecorder()
	b.ServeHTTP(spend, signedReq(t, priv, "POST", "/io.pilot.test/echo", []byte(`{}`), later))
	if spend.Code != http.StatusOK {
		t.Fatalf("/echo: status %d, want 200 (body %s)", spend.Code, spend.Body.String())
	}
	bal, rec = balanceOf(t, b, priv, later)
	if rec.Code != http.StatusOK || bal != 90 {
		t.Fatalf("post-spend balance = %d (status %d), want 90", bal, rec.Code)
	}
}

// TestBalanceRateLimit_BurstThenSheds pins allowBalanceRead's light, generous
// token bucket: legitimate back-to-back balance checks (well within
// balanceBurst) always succeed, but a tight loop well past the burst
// eventually gets a 429 distinct from the mint-cooldown error, and recovers
// once the fixed clock is advanced past a refill interval.
func TestBalanceRateLimit_BurstThenSheds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, _ := creditBroker(t, 200, now) // no mint_cooldown_ms at all
	_, priv := newKey(t)

	shed := false
	var lastCode int
	for i := 0; i < balanceBurst+10; i++ {
		_, rec := balanceOf(t, b, priv, now)
		lastCode = rec.Code
		if rec.Code == http.StatusTooManyRequests {
			shed = true
			break
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: unexpected status %d (body %s)", i, rec.Code, rec.Body.String())
		}
	}
	if !shed {
		t.Fatalf("expected the burst limiter to eventually 429 within %d calls, last status %d", balanceBurst+10, lastCode)
	}

	// Advance the clock well past a full refill and confirm it recovers.
	later := now.Add(10 * time.Second)
	b.Verify = VerifyConfig{Now: fixedClock(later)}
	_, rec := balanceOf(t, b, priv, later)
	if rec.Code != http.StatusOK {
		t.Fatalf("after refill: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestBalancePath_SharedConstant_NoDrift pins finding #3: scaffold's
// BalanceMetaPath and the broker's pilotBalancePath must be the exact same
// string, sourced from one place (pilotpath.Balance) rather than two
// independently-typed literals that could silently diverge.
func TestBalancePath_SharedConstant_NoDrift(t *testing.T) {
	if pilotBalancePath != pilotpath.Balance {
		t.Fatalf("broker.pilotBalancePath = %q, want pilotpath.Balance %q — the broker's route and the shared constant have drifted apart", pilotBalancePath, pilotpath.Balance)
	}
}

// TestBalanceMeta_CrossCallerIsolation pins finding #4: two distinct signed
// identities calling the SAME app's `.balance` route each see only their OWN
// remaining budget, never the other's (or the shared master account's pooled
// balance).
func TestBalanceMeta_CrossCallerIsolation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, _ := creditBroker(t, 200, now) // seed_credits 100, /echo costs 40
	_, alicePriv := newKey(t)
	_, bobPriv := newKey(t)

	// Alice seeds to 100, then spends 40 on /echo -> 60 remaining.
	aliceBal, rec := balanceOf(t, b, alicePriv, now)
	if rec.Code != http.StatusOK || aliceBal != 100 {
		t.Fatalf("alice seed balance = %d (status %d), want 100", aliceBal, rec.Code)
	}
	spend := httptest.NewRecorder()
	b.ServeHTTP(spend, signedReq(t, alicePriv, "POST", "/io.pilot.test/echo", []byte(`{}`), now))
	if spend.Code != http.StatusOK {
		t.Fatalf("alice /echo: status %d, want 200", spend.Code)
	}
	aliceBal, rec = balanceOf(t, b, alicePriv, now)
	if rec.Code != http.StatusOK || aliceBal != 60 {
		t.Fatalf("alice post-spend balance = %d (status %d), want 60", aliceBal, rec.Code)
	}

	// Bob, a completely distinct identity on the same app, is seeded fresh to
	// 100 — Alice's spend must not be visible to him, and his own reads must
	// never move Alice's balance.
	bobBal, rec := balanceOf(t, b, bobPriv, now)
	if rec.Code != http.StatusOK || bobBal != 100 {
		t.Fatalf("bob balance = %d (status %d), want 100 (own fresh seed, unaffected by alice's spend)", bobBal, rec.Code)
	}
	for i := 0; i < 2; i++ {
		bobBal, rec = balanceOf(t, b, bobPriv, now)
		if rec.Code != http.StatusOK || bobBal != 100 {
			t.Fatalf("bob repeat balance = %d (status %d), want 100", bobBal, rec.Code)
		}
	}

	// Alice's balance is unchanged by any of Bob's calls.
	aliceBal, rec = balanceOf(t, b, alicePriv, now)
	if rec.Code != http.StatusOK || aliceBal != 60 {
		t.Fatalf("alice balance after bob's activity = %d (status %d), want 60 (unaffected)", aliceBal, rec.Code)
	}
}
