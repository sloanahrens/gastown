package daemon

import (
	"testing"
	"time"
)

const testStaleFor = 5 * time.Minute

// TestDeaconCycleTracker_Baseline covers the first sample: it can only record
// the cycle, since the daemon has no way to know how long that cycle had
// already been current before it started looking.
func TestDeaconCycleTracker_Baseline(t *testing.T) {
	var tr deaconCycleTracker
	now := time.Now()

	obs := tr.observe(7, testStaleFor, now)
	if obs.Age != 0 || obs.Stalled || obs.StalledTicks != 0 {
		t.Errorf("first sample = %+v, want zero observation", obs)
	}
	if tr.cycle != 7 || !tr.known || !tr.changedAt.Equal(now) {
		t.Errorf("baseline = %+v, want cycle 7 recorded at %s", tr, now)
	}
}

// TestDeaconCycleTracker_AgesStaticCycle covers the samples after the
// baseline: the same cycle ages, a new cycle re-baselines.
func TestDeaconCycleTracker_AgesStaticCycle(t *testing.T) {
	var tr deaconCycleTracker
	start := time.Now()
	tr.observe(7, testStaleFor, start)

	if obs := tr.observe(7, testStaleFor, start.Add(2*time.Minute)); obs.Stalled || obs.Age != 2*time.Minute {
		t.Errorf("after 2m on the same cycle = %+v, want age 2m0s and not stalled", obs)
	}

	// Crossing the threshold reports a stall and counts the sample.
	obs := tr.observe(7, testStaleFor, start.Add(6*time.Minute))
	if !obs.Stalled || obs.StalledTicks != 1 || obs.Age != 6*time.Minute {
		t.Errorf("after 6m on the same cycle = %+v, want stalled, 1 tick, age 6m0s", obs)
	}
	obs = tr.observe(7, testStaleFor, start.Add(11*time.Minute))
	if !obs.Stalled || obs.StalledTicks != 2 {
		t.Errorf("second stalled sample = %+v, want 2 consecutive ticks", obs)
	}

	// An advancing cycle clears the stall and re-baselines.
	obs = tr.observe(8, testStaleFor, start.Add(12*time.Minute))
	if obs.Age != 0 || obs.Stalled || obs.StalledTicks != 0 {
		t.Errorf("advancing cycle = %+v, want a fresh baseline", obs)
	}
	if tr.stalledTicks != 0 {
		t.Errorf("stalledTicks = %d after the cycle advanced, want 0", tr.stalledTicks)
	}
}

// TestDeaconCycleTracker_StallCounterResetsBelowThreshold verifies that the
// consecutive-sample count only counts samples which actually found a stall: an
// occasional sample inside the window breaks the run, so two "consecutive"
// ticks means two samples that both saw the cycle past the threshold.
func TestDeaconCycleTracker_StallCounterResetsBelowThreshold(t *testing.T) {
	var tr deaconCycleTracker
	start := time.Now()
	tr.observe(7, testStaleFor, start)

	tr.observe(7, testStaleFor, start.Add(6*time.Minute)) // stalled, run = 1
	obs := tr.observe(8, testStaleFor, start.Add(7*time.Minute))
	if obs.StalledTicks != 0 {
		t.Errorf("StalledTicks = %d after the cycle advanced, want 0", obs.StalledTicks)
	}
	obs = tr.observe(8, testStaleFor, start.Add(9*time.Minute)) // 2m on cycle 8
	if obs.Stalled || obs.StalledTicks != 0 {
		t.Errorf("sample inside the window = %+v, want not stalled", obs)
	}
	obs = tr.observe(8, testStaleFor, start.Add(14*time.Minute)) // 7m on cycle 8
	if !obs.Stalled || obs.StalledTicks != 1 {
		t.Errorf("sample past the window = %+v, want a single stalled tick", obs)
	}
}

// TestDeaconCycleTracker_Reset verifies that a reset drops everything the
// tracker knew, so a gap between observations cannot be reported as a stall.
func TestDeaconCycleTracker_Reset(t *testing.T) {
	tr := deaconCycleTracker{
		cycle:        7,
		changedAt:    time.Now().Add(-time.Hour),
		known:        true,
		stalledTicks: 4,
	}

	tr.reset()

	if tr.known || tr.cycle != 0 || !tr.changedAt.IsZero() || tr.stalledTicks != 0 {
		t.Errorf("tracker = %+v after reset, want the zero value", tr)
	}
}
