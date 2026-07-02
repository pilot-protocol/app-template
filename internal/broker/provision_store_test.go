package broker

import (
	"sync"
	"testing"
	"time"
)

// provisionStores returns the two impls under test so every case runs against both.
func provisionStores(t *testing.T) map[string]ProvisionStore {
	t.Helper()
	sq, err := OpenSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	return map[string]ProvisionStore{"mem": NewMemStore(), "sqlite": sq}
}

func TestProvision_SeedOnceIdempotent(t *testing.T) {
	for name, st := range provisionStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1_000_000, 0)
			r1, err := st.Provision("io.pilot.smol", "alice", "1.2.3.4", 5, 0, 0, now)
			if err != nil || !r1.New || r1.Credits != 5 {
				t.Fatalf("first provision: %+v err=%v", r1, err)
			}
			// spend one, then re-provision: must NOT re-seed.
			if ok, _, _ := st.Debit("io.pilot.smol", "alice", 1); !ok {
				t.Fatal("debit")
			}
			r2, err := st.Provision("io.pilot.smol", "alice", "1.2.3.4", 5, 0, 0, now.Add(time.Hour))
			if err != nil {
				t.Fatalf("re-provision err: %v", err)
			}
			if r2.New {
				t.Fatal("re-provision must not create a new row")
			}
			if r2.Credits != 4 {
				t.Fatalf("re-provision must not re-seed: got %d want 4", r2.Credits)
			}
		})
	}
}

func TestProvision_PerIPCap(t *testing.T) {
	for name, st := range provisionStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(2_000_000, 0)
			ip := "9.9.9.9"
			for i, caller := range []string{"a", "b", "c"} {
				_, err := st.Provision("app", caller, ip, 1, 3, 0, now)
				if err != nil {
					t.Fatalf("provision %d unexpected err: %v", i, err)
				}
			}
			// 4th distinct identity on the same IP trips the cap.
			if _, err := st.Provision("app", "d", ip, 1, 3, 0, now); err != ErrIPCap {
				t.Fatalf("want ErrIPCap, got %v", err)
			}
			// a different IP is unaffected.
			if _, err := st.Provision("app", "e", "8.8.8.8", 1, 3, 0, now); err != nil {
				t.Fatalf("different IP should be allowed: %v", err)
			}
		})
	}
}

func TestProvision_Cooldown(t *testing.T) {
	for name, st := range provisionStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(3_000_000, 0)
			if _, err := st.Provision("app", "alice", "ip", 1, 0, time.Minute, now); err != nil {
				t.Fatal(err)
			}
			// re-mint within the cooldown → rejected.
			if _, err := st.Provision("app", "alice", "ip", 1, 0, time.Minute, now.Add(30*time.Second)); err != ErrCooldown {
				t.Fatalf("want ErrCooldown, got %v", err)
			}
			// after the cooldown → allowed.
			if _, err := st.Provision("app", "alice", "ip", 1, 0, time.Minute, now.Add(2*time.Minute)); err != nil {
				t.Fatalf("after cooldown should pass: %v", err)
			}
		})
	}
}

func TestDebit_AtomicNeverOversell(t *testing.T) {
	for name, st := range provisionStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(4_000_000, 0)
			if _, err := st.Provision("app", "alice", "ip", 100, 0, 0, now); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			ok := 0
			for i := 0; i < 200; i++ { // 200 concurrent debits of 1 against 100 credits
				wg.Add(1)
				go func() {
					defer wg.Done()
					if got, _, _ := st.Debit("app", "alice", 1); got {
						mu.Lock()
						ok++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if ok != 100 {
				t.Fatalf("exactly 100 debits should succeed, got %d", ok)
			}
			if c, _ := st.Credit("app", "alice"); c != 0 {
				t.Fatalf("balance should be 0, got %d", c)
			}
		})
	}
}

func TestDebit_ZeroAndRefund(t *testing.T) {
	for name, st := range provisionStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(5_000_000, 0)
			if _, err := st.Provision("app", "bob", "ip", 2, 0, 0, now); err != nil {
				t.Fatal(err)
			}
			// overspend rejected, balance unchanged.
			if ok, rem, _ := st.Debit("app", "bob", 5); ok || rem != 2 {
				t.Fatalf("overspend must fail leaving balance: ok=%v rem=%d", ok, rem)
			}
			// drain to zero, then 402.
			st.Debit("app", "bob", 2)
			if ok, _, _ := st.Debit("app", "bob", 1); ok {
				t.Fatal("debit at zero must fail")
			}
			// refund restores.
			st.Refund("app", "bob", 1)
			if c, _ := st.Credit("app", "bob"); c != 1 {
				t.Fatalf("refund should restore to 1, got %d", c)
			}
		})
	}
}

func TestConcurrentFirstUse_SingleSeed(t *testing.T) {
	for name, st := range provisionStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(6_000_000, 0)
			var wg sync.WaitGroup
			var mu sync.Mutex
			news := 0
			for i := 0; i < 30; i++ { // many concurrent first-use provisions of the same caller
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := st.Provision("app", "carol", "ip", 10, 0, 0, now)
					if err == nil && r.New {
						mu.Lock()
						news++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if news != 1 {
				t.Fatalf("exactly one provision should seed, got %d", news)
			}
			if c, _ := st.Credit("app", "carol"); c != 10 {
				t.Fatalf("balance should be a single seed of 10, got %d", c)
			}
		})
	}
}

func TestOwnerNameSanitizesCloudInvalidChars(t *testing.T) {
	// A base64 caller id with '+' and '/' must yield a cloud-valid machine name
	// (letters, digits, '-', '_' only) while the owner tag keeps the exact id.
	got := ownerName("zKg+3BS8/bt7", "my+vm/1")
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Fatalf("machine name %q contains cloud-invalid char %q", got, r)
		}
	}
	if got != "smol-zKg-3BS8-my-vm-1" {
		t.Fatalf("unexpected sanitized name: %q", got)
	}
}
