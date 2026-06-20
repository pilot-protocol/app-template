package broker

import (
	"sync"
	"testing"
)

// storeFactory builds a fresh Store for the contract tests. Both the in-memory
// and SQLite stores must satisfy the same contract (Liskov): the broker should
// not care which is wired in.
type storeFactory struct {
	name string
	make func(t *testing.T) Store
}

func storeFactories(t *testing.T) []storeFactory {
	return []storeFactory{
		{"mem", func(t *testing.T) Store { return NewMemStore() }},
		{"sqlite", func(t *testing.T) Store {
			s, err := OpenSQLiteStore(":memory:")
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	}
}

func TestStoreContract_AdmitCountsAndMeters(t *testing.T) {
	for _, f := range storeFactories(t) {
		t.Run(f.name, func(t *testing.T) {
			s := f.make(t)
			ok, n := s.Admit("app", "alice", 2)
			if !ok || n != 1 {
				t.Fatalf("first admit = (%v,%d), want (true,1)", ok, n)
			}
			ok, n = s.Admit("app", "alice", 2)
			if !ok || n != 2 {
				t.Fatalf("second admit = (%v,%d), want (true,2)", ok, n)
			}
			ok, _ = s.Admit("app", "alice", 2)
			if ok {
				t.Fatal("third admit should be denied (quota 2)")
			}
			s.AddCost("app", "alice", 7.5)
			calls, cents := s.Usage("app", "alice")
			if calls != 2 || cents != 7.5 {
				t.Fatalf("usage = (%d,%.1f), want (2,7.5)", calls, cents)
			}
		})
	}
}

func TestStoreContract_PerCallerAndPerApp(t *testing.T) {
	for _, f := range storeFactories(t) {
		t.Run(f.name, func(t *testing.T) {
			s := f.make(t)
			s.Admit("app1", "alice", 0)
			s.Admit("app1", "bob", 0)
			s.Admit("app2", "alice", 0)
			if c, _ := s.Usage("app1", "alice"); c != 1 {
				t.Fatalf("app1/alice = %d, want 1", c)
			}
			if c, _ := s.Usage("app1", "bob"); c != 1 {
				t.Fatalf("app1/bob = %d, want 1", c)
			}
			if c, _ := s.Usage("app2", "alice"); c != 1 {
				t.Fatalf("app2/alice = %d, want 1", c)
			}
		})
	}
}

func TestStoreContract_UnlimitedQuota(t *testing.T) {
	for _, f := range storeFactories(t) {
		t.Run(f.name, func(t *testing.T) {
			s := f.make(t)
			for i := 0; i < 100; i++ {
				if ok, _ := s.Admit("app", "alice", 0); !ok {
					t.Fatalf("unlimited quota denied at call %d", i)
				}
			}
		})
	}
}

// Admit must be atomic: N concurrent callers against quota N all succeed, and
// the (N+1)th is denied — no double-admit past the cap.
func TestStoreContract_AdmitIsAtomic(t *testing.T) {
	for _, f := range storeFactories(t) {
		t.Run(f.name, func(t *testing.T) {
			s := f.make(t)
			const quota = 50
			var wg sync.WaitGroup
			var mu sync.Mutex
			admitted := 0
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if ok, _ := s.Admit("app", "alice", quota); ok {
						mu.Lock()
						admitted++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if admitted != quota {
				t.Fatalf("admitted %d under concurrency, want exactly %d", admitted, quota)
			}
		})
	}
}

// SQLite usage must survive reopening the same file.
func TestSQLiteStore_Durable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/usage.db"
	s1, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Admit("app", "alice", 0)
	s1.AddCost("app", "alice", 3)
	_ = s1.Close()

	s2, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	calls, cents := s2.Usage("app", "alice")
	if calls != 1 || cents != 3 {
		t.Fatalf("after reopen usage = (%d,%.0f), want (1,3) — not durable", calls, cents)
	}
}
