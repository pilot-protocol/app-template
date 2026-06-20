package broker

import (
	"fmt"
	"testing"
)

// BenchmarkSQLiteAdmit measures the durable store's quota check-and-count (one
// write txn per call) — the broker's per-request metering cost.
func BenchmarkSQLiteAdmit(b *testing.B) {
	s, err := OpenSQLiteStore(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Admit("app", "caller", 0) // unlimited: every call counts (worst case)
	}
}

// BenchmarkSQLiteAdmitConcurrent measures throughput with many goroutines
// contending on the single writer (the realistic multi-user shape).
func BenchmarkSQLiteAdmitConcurrent(b *testing.B) {
	s, err := OpenSQLiteStore(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Admit("app", fmt.Sprintf("caller-%d", i%100), 0)
			i++
		}
	})
}

// BenchmarkSQLiteAdmitDisk measures Admit against an on-disk DB (WAL) — the real
// prod path that actually fsyncs, unlike the :memory: benches above.
func BenchmarkSQLiteAdmitDisk(b *testing.B) {
	s, err := OpenSQLiteStore(b.TempDir() + "/usage.db")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Admit("app", "caller", 0)
	}
}

// BenchmarkMemAdmit is the in-memory baseline for comparison.
func BenchmarkMemAdmit(b *testing.B) {
	s := NewMemStore()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Admit("app", "caller", 0)
		}
	})
}

// BenchmarkSQLiteFullCall mimics one brokered call's store work: Admit + AddCost.
func BenchmarkSQLiteFullCall(b *testing.B) {
	s, err := OpenSQLiteStore(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Admit("app", "caller", 0)
		s.AddCost("app", "caller", 1.5)
	}
}
