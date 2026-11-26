package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestGCLandingHoldHeldAroundTheRun: a due gc takes its own hold on new landing
// passes for the run. With the passes in flight already ended (the fake
// waitIdle), the hold is on while the gc call runs and released after it, and
// the gc is not deferred (gt-j5q02z).
func TestGCLandingHoldHeldAroundTheRun(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}

	waited := 0
	var order []string
	d.maint.waitIdle = func(*Daemon, <-chan struct{}) bool {
		waited++
		order = append(order, "wait")
		return true
	}
	heldDuringGC := false
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		order = append(order, "gc")
		heldDuringGC = d.landingHolds.heldBy(landingHoldMaintenanceGC)
		return nil
	}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed — the wait was satisfied, so the gc must run", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Fatalf("gc calls = %s, want hq", got)
	}
	if waited != 1 {
		t.Errorf("waitIdle called %d times, want once before the gc", waited)
	}
	if got := strings.Join(order, ","); got != "wait,gc" {
		t.Errorf("order = %s, want the wait before the gc call", got)
	}
	if !heldDuringGC {
		t.Error("the maintenance-gc hold was not held while the gc call ran")
	}
	if d.landingHolds.heldBy(landingHoldMaintenanceGC) {
		t.Error("the maintenance-gc hold is still held after the cycle returned")
	}
}

// TestGCLandingHoldWaitTimesOut: a landing pass that outlasts the bound defers
// the gc instead of running it, and the gc's landing hold is released (the
// timeout branch of maintenanceGCWaitIdle, driven by an injected timer that is
// already fired: no real time passes). gt-j5q02z.
func TestGCLandingHoldWaitTimesOut(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	d.landingPasses.Store(1) // a pass in flight that never ends in this test
	fired := make(chan time.Time, 1)
	fired <- time.Time{}
	var bound time.Duration
	d.maint.holdTimer = func(dur time.Duration) (<-chan time.Time, func()) {
		bound = dur
		return fired, func() {}
	}
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		return nil
	}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred: the pass outlasted the bound", out.outcome)
	}
	if want := "landing pass did not end within " + maintenanceGCHoldBound.String(); !strings.Contains(out.reason, want) {
		t.Errorf("reason = %q, want it to contain %q", out.reason, want)
	}
	if bound != maintenanceGCHoldBound {
		t.Errorf("the wait's timer was started for %v, want %v", bound, maintenanceGCHoldBound)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v, want none after a timed-out wait", f.gcCalls)
	}
	if d.landingHolds.heldBy(landingHoldMaintenanceGC) {
		t.Error("the maintenance-gc hold is still held after the deferred cycle returned")
	}
}

// TestGCLandingHoldWaitEndsWhenThePassesEnd: with a pass in flight the wait
// returns true when the hold's idle channel closes, before its timer fires, and
// the gc runs after it (the non-zero branch of maintenanceGCWaitIdle).
func TestGCLandingHoldWaitEndsWhenThePassesEnd(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.landingPasses.Store(1)
	never := make(chan time.Time) // the timer never fires
	d.maint.holdTimer = func(time.Duration) (<-chan time.Time, func()) { return never, func() {} }
	idle := make(chan struct{})
	close(idle)

	if !d.maintenanceGCWaitIdle(idle) {
		t.Error("the wait gave up although the passes had ended")
	}
	d.landingPasses.Store(0)
	// With no pass in flight the wait returns at once. The timer is the fired
	// one, so a wait that wrongly blocks on the idle channel ends on it and the
	// row fails instead of hanging.
	fired := make(chan time.Time, 1)
	fired <- time.Time{}
	d.maint.holdTimer = func(time.Duration) (<-chan time.Time, func()) { return fired, func() {} }
	if !d.maintenanceGCWaitIdle(make(chan struct{})) {
		t.Error("the wait did not return at once with no pass in flight")
	}
}

// TestGCLandingHoldReleasedOnEveryDeferral: a cycle that gives up at the quiet
// guard, or at the Dolt lock wait, returns with the maintenance-gc hold
// released, so a landing pass can start again (gt-j5q02z).
func TestGCLandingHoldReleasedOnEveryDeferral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		arrange func(d *Daemon, f *gcFakes)
		reason  string
	}{
		{"the quiet guard says busy", func(_ *Daemon, f *gcFakes) { f.quietUntil = 0 }, ""},
		{"the Dolt lock wait runs out", func(d *Daemon, _ *gcFakes) { d.doltMaintMu.RLock() }, "daemon Dolt task in flight"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := gcTestDaemon(t)
			f := withGCFakes(t, d)
			f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
			tt.arrange(d, f)
			d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
				f.gcCalls = append(f.gcCalls, db)
				return nil
			}

			out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

			if out.outcome != gcOutcomeDeferred {
				t.Fatalf("outcome = %v, want deferred", out.outcome)
			}
			if tt.reason != "" && out.reason != tt.reason {
				t.Errorf("reason = %q, want %q", out.reason, tt.reason)
			}
			if len(f.gcCalls) != 0 {
				t.Errorf("gc calls = %v, want none after a deferral", f.gcCalls)
			}
			if d.landingHolds.heldBy(landingHoldMaintenanceGC) {
				t.Error("the maintenance-gc hold is still held after the deferred cycle returned")
			}
		})
	}
}

// TestGCLandingHoldTimeoutDefers: a wait that reports the landing passes did
// not end within maintenanceGCHoldBound defers the cycle with the bound named,
// runs no gc, and releases the hold on that exit. The seam answers, so no real
// time passes (gt-eysfig).
func TestGCLandingHoldTimeoutDefers(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	waited := 0
	heldForWait := false
	d.maint.waitIdle = func(dd *Daemon, _ <-chan struct{}) bool {
		waited++
		heldForWait = dd.landingHolds.heldBy(landingHoldMaintenanceGC)
		return false
	}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)
	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred: the wait gave up", out.outcome)
	}
	if want := "landing pass did not end within 15m0s"; out.reason != want {
		t.Errorf("reason = %q, want %q", out.reason, want)
	}
	if waited != 1 {
		t.Errorf("waitIdle called %d times, want once before the gc", waited)
	}
	if !heldForWait {
		t.Error("the maintenance-gc hold was not held when the wait ran")
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v, want none after a timed-out wait", f.gcCalls)
	}
	if d.landingHolds.heldBy(landingHoldMaintenanceGC) {
		t.Error("the maintenance-gc hold is still held after the deferred cycle returned")
	}
}

// TestGCLandingHoldReleasedOnEveryExit: the maintenance-gc hold is released
// after a gc that fails, and a cycle with nothing due returns before the hold,
// leaving a hold another caller took as it found it (gt-eysfig).
func TestGCLandingHoldReleasedOnEveryExit(t *testing.T) {
	t.Parallel()
	t.Run("a gc that fails", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
		f.gcErr["hq"] = errors.New("dolt_gc failed")

		out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)
		if out.outcome != gcOutcomeFailed {
			t.Fatalf("outcome = %v, want failed", out.outcome)
		}
		if d.landingHolds.heldBy(landingHoldMaintenanceGC) {
			t.Error("the maintenance-gc hold is still held after a failed gc returned")
		}
	})
	t.Run("nothing due", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		f := withGCFakes(t, d)
		f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
		recordRun(t, d, "hq", 1, 0) // a patrol gc yesterday: not due
		// Another caller holds maintenance-gc; a cycle with nothing due returns
		// before it takes the hold, so it cannot release this one.
		d.landingHolds.hold(landingHoldMaintenanceGC)

		out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)
		if out.outcome != gcOutcomeCompleted {
			t.Fatalf("outcome = %v, want completed", out.outcome)
		}
		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v, want none for a database that is not due", f.gcCalls)
		}
		if !d.landingHolds.heldBy(landingHoldMaintenanceGC) {
			t.Error("a cycle with nothing due took and released the maintenance-gc hold")
		}
		d.landingHolds.release(landingHoldMaintenanceGC)
	})
}

// TestGCLandingHoldQuietStillGuards: with the landing wait satisfied, a gate
// slot held by another role still defers the database — a satisfied wait does
// not skip the guard, and the gc's own hold is not hard work to it (gt-eysfig).
func TestGCLandingHoldQuietStillGuards(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	d.maint.waitIdle = func(*Daemon, <-chan struct{}) bool { return true }
	d.maint.slotHolders = func(string) ([]string, error) { return []string{"refinery"}, nil }
	heldForQuiet := false
	d.maint.quiet = func(dd *Daemon) (bool, string) { // the real guard, watched
		heldForQuiet = dd.landingHolds.heldBy(landingHoldMaintenanceGC)
		return dd.maintenanceQuiet()
	}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)
	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred by the quiet check", out.outcome)
	}
	if want := "slot held by refinery"; out.reason != want {
		t.Errorf("reason = %q, want %q", out.reason, want)
	}
	if !heldForQuiet {
		t.Error("the maintenance-gc hold was not held when the quiet check ran")
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v, want none while a gate slot is held", f.gcCalls)
	}
	if d.landingHolds.heldBy(landingHoldMaintenanceGC) {
		t.Error("the maintenance-gc hold is still held after the deferred cycle returned")
	}
}
