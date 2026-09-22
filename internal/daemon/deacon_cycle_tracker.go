package daemon

import "time"

// deaconCycleTracker dates the Deacon's heartbeat cycle, so a timestamp kept
// fresh by the heartbeat poller cannot hide a cycle that has stopped moving
// (gt-t3cw).
//
// In-memory by design: a fresh daemon re-baselines from the first heartbeat it
// reads, rather than inheriting stall evidence from before its own start.
type deaconCycleTracker struct {
	cycle     int64
	changedAt time.Time
	known     bool

	// stalledTicks counts consecutive samples that found the same cycle past
	// the stale threshold, so one sample cannot trigger a restart.
	stalledTicks int
}

// cycleObservation is one sample of the Deacon's heartbeat cycle.
type cycleObservation struct {
	// Age is how long the observed cycle has been unchanged, zero on the
	// sample that establishes or advances the baseline.
	Age time.Duration

	// Stalled reports whether Age has reached the stale threshold passed to
	// observe.
	Stalled bool

	// StalledTicks is the number of consecutive samples, including this one,
	// that found the cycle stalled.
	StalledTicks int
}

// observe records the cycle from the current heartbeat and reports how long
// that cycle has been unchanged. A changed cycle (or the first sample ever)
// re-baselines and reports an age of zero.
func (t *deaconCycleTracker) observe(cycle int64, staleFor time.Duration, now time.Time) cycleObservation {
	if !t.known || cycle != t.cycle {
		t.cycle = cycle
		t.changedAt = now
		t.known = true
		t.stalledTicks = 0
		return cycleObservation{}
	}

	age := now.Sub(t.changedAt)
	if age < staleFor {
		t.stalledTicks = 0
		return cycleObservation{Age: age}
	}

	t.stalledTicks++
	return cycleObservation{Age: age, Stalled: true, StalledTicks: t.stalledTicks}
}

// reset drops the baseline. Called whenever a gap in observations is explained
// by something other than a stall — no heartbeat file to read, or a
// crash-loop hold that skips this check entirely. Without it, the age reported
// on the next check measures the gap rather than the stall.
func (t *deaconCycleTracker) reset() {
	*t = deaconCycleTracker{}
}
