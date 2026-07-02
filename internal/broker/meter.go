package broker

import (
	"context"
	"log"
	"time"
)

// Usage metering: credit is micro-dollars and drains by REAL usage. A background
// loop (RunMeter) periodically attributes each running machine's cost
// (resources × the rate card × elapsed time) to its owner and STOPS an owner's
// machines once their balance is exhausted — so an agent gets exactly what it
// pays for.

type meterRates struct {
	cpuHour, memGbHour, diskGbHour int64 // micro-$ per hour
}

func (a *AppEntry) meterRates() meterRates {
	p := a.Provision
	return meterRates{p.CpuHourMicros, p.MemGbHourMicros, p.DiskGbHourMicros}
}

func (r meterRates) enabled() bool { return r.cpuHour > 0 || r.memGbHour > 0 || r.diskGbHour > 0 }

// costMicros is the micro-$ a machine accrues over dur at these rates.
func (r meterRates) costMicros(m MachineInfo, dur time.Duration) int64 {
	perHour := float64(r.cpuHour)*m.Cpus +
		float64(r.memGbHour)*(m.MemoryMb/1024) +
		float64(r.diskGbHour)*m.DiskGb
	return int64(perHour * dur.Hours())
}

// MeterOnce runs one metering tick for a provisioned app: charge each running
// machine's usage-since-dur to its owner (draining, never negative) and stop the
// machines of any owner whose balance is exhausted. Returns machines stopped.
func (b *Broker) MeterOnce(ctx context.Context, app *AppEntry, dur time.Duration) (stopped int, err error) {
	ps, ok := b.provStore()
	if !ok || app.Provision == nil || app.provider == nil {
		return 0, nil
	}
	rates := app.meterRates()
	if !rates.enabled() {
		return 0, nil
	}
	machines, err := app.provider.AllOwned(ctx)
	if err != nil {
		return 0, err
	}
	for _, m := range machines {
		if m.Owner == "" || !m.Running() {
			continue
		}
		rec, exists, _ := ps.Get(app.ID, m.Owner)
		if !exists {
			continue
		}
		bal := rec.Credits
		if bal > 0 {
			charge := int(rates.costMicros(m, dur))
			if charge > bal {
				charge = bal // drain to zero, never negative
			}
			if charge > 0 {
				ps.Debit(app.ID, m.Owner, charge)
				bal -= charge
			}
		}
		if bal <= 0 { // exhausted → stop the machine (you get what you pay for)
			if serr := app.provider.Stop(ctx, m.ID); serr == nil {
				stopped++
				log.Printf("meter: %s: stopped %s (owner %.8s… out of credit)", app.ID, m.ID, m.Owner)
			}
		}
	}
	return stopped, nil
}

// RunMeter is the metering loop: every interval it charges real usage across all
// provisioned apps in the current registry. Runs until ctx is cancelled.
func (b *Broker) RunMeter(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	last := b.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := b.now()
			dur := now.Sub(last)
			last = now
			for _, app := range b.reg.Load().apps {
				if app.Provision == nil || app.Provision.MeterIntervalMs < 0 {
					continue // metering disabled for this app
				}
				if _, err := b.MeterOnce(ctx, app, dur); err != nil {
					log.Printf("meter: %s: %v", app.ID, err)
				}
			}
		}
	}
}
