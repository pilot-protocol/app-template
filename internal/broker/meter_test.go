package broker

import (
	"context"
	"testing"
	"time"
)

// fakeProvider is a metering-only CloudProvider for the meter tests.
type fakeProvider struct {
	machines []MachineInfo
	stopped  map[string]bool
}

func (f *fakeProvider) Push(context.Context, string, PushSpec, []byte) (CloudResp, error) {
	return CloudResp{}, nil
}
func (f *fakeProvider) List(context.Context, string) (CloudResp, error) { return CloudResp{}, nil }
func (f *fakeProvider) AllOwned(context.Context) ([]MachineInfo, error) { return f.machines, nil }
func (f *fakeProvider) Stop(_ context.Context, id string) error {
	f.stopped[id] = true
	for i := range f.machines {
		if f.machines[i].ID == id {
			f.machines[i].State = "stopped"
		}
	}
	return nil
}
func (f *fakeProvider) Name() string { return "fake" }

func meterApp(fp *fakeProvider) *AppEntry {
	return &AppEntry{
		ID: "io.pilot.smol",
		Provision: &ProvisionSpec{
			CpuHourMicros: 43200, MemGbHourMicros: 16200, DiskGbHourMicros: 100,
		},
		provider: fp,
	}
}

func TestMeter_DrainsByRealUsageAndStops(t *testing.T) {
	st := NewMemStore()
	now := time.Unix(1_000_000, 0)
	// alice seeded with 100_000 micro-$ ($0.10)
	if _, err := st.Provision("io.pilot.smol", "alice", "ip", 100_000, 0, 0, now); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{stopped: map[string]bool{},
		machines: []MachineInfo{{ID: "mach-1", Owner: "alice", State: "started", Cpus: 4, MemoryMb: 8192}}}
	b := New(&Registry{apps: map[string]*AppEntry{}}, st)
	app := meterApp(fp)

	// rate: 4*43200 + 8*16200 = 302400 micro-$/hour. Over 1 hour that's > balance
	// (100_000), so credit drains to 0 and the machine is stopped.
	stopped, err := b.MeterOnce(context.Background(), app, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stopped != 1 || !fp.stopped["mach-1"] {
		t.Fatalf("exhausted machine must be stopped, stopped=%d map=%v", stopped, fp.stopped)
	}
	if c, _ := st.Credit("io.pilot.smol", "alice"); c != 0 {
		t.Fatalf("credit must drain to 0, got %d", c)
	}
}

func TestMeter_PartialChargeKeepsRunning(t *testing.T) {
	st := NewMemStore()
	now := time.Unix(2_000_000, 0)
	st.Provision("io.pilot.smol", "bob", "ip", 5_000_000, 0, 0, now) // $5
	fp := &fakeProvider{stopped: map[string]bool{},
		machines: []MachineInfo{{ID: "m", Owner: "bob", State: "started", Cpus: 1, MemoryMb: 1024}}}
	b := New(&Registry{apps: map[string]*AppEntry{}}, st)
	app := meterApp(fp)

	// 1 cpu + 1 GB for 6 minutes (0.1h): (43200 + 16200)*0.1 = 5940 micro-$.
	stopped, err := b.MeterOnce(context.Background(), app, 6*time.Minute)
	if err != nil || stopped != 0 {
		t.Fatalf("well-funded machine must keep running: stopped=%d err=%v", stopped, err)
	}
	if c, _ := st.Credit("io.pilot.smol", "bob"); c != 5_000_000-5940 {
		t.Fatalf("expected 4_994_060 remaining, got %d", c)
	}
}
