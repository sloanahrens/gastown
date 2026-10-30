package daemon

import "testing"

// TestLandingHoldsReleaseOnlyTheirOwnName pins the shared drain both jobs use:
// a released hold leaves the other one on, so a restart that gives up does not
// free a pass while rebuild_om is still swapping the om (gt-0hsf6).
func TestLandingHoldsReleaseOnlyTheirOwnName(t *testing.T) {
	t.Parallel()
	var holds landingHolds
	holds.hold(landingHoldUpgradeRestart)
	holds.hold(landingHoldRebuildOM)

	holds.release(landingHoldRebuildOM)

	if holds.heldBy(landingHoldRebuildOM) {
		t.Error("the released hold is still taken")
	}
	if !holds.heldBy(landingHoldUpgradeRestart) {
		t.Error("releasing one hold released the other")
	}
	if !holds.held() {
		t.Error("no hold is left, so a landing pass may start")
	}

	holds.release(landingHoldUpgradeRestart)
	if holds.held() {
		t.Error("the last release left a hold behind")
	}
}

// TestLandingHoldWakesWhenThePassInFlightEnds pins the wake a holder waits on:
// the channel it was handed closes when the in-flight count reaches zero, and
// not before.
func TestLandingHoldWakesWhenThePassInFlightEnds(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.landingPasses.Store(1)
	idle := d.landingHolds.hold(landingHoldRebuildOM)

	select {
	case <-idle:
		t.Fatal("woke with a pass still in flight")
	default:
	}

	d.landingPassEnded()

	select {
	case <-idle:
	default:
		t.Fatal("a pass ended and the holder was not woken")
	}
}

// TestBeginLandingPassRefusesAPassAfterAHoldGoesOn pins the recheck that closes
// the drain's window: a pass whose first check ran before the hold does not
// start beside the install the holder waits for, and it leaves no count behind
// (gt-0hsf6).
func TestBeginLandingPassRefusesAPassAfterAHoldGoesOn(t *testing.T) {
	t.Parallel()
	d := &Daemon{}

	if !d.beginLandingPass() {
		t.Fatal("a quiet town refused a landing pass")
	}
	d.landingPassEnded()
	if got := d.landingPasses.Load(); got != 0 {
		t.Fatalf("in-flight count = %d after one pass ended, want 0", got)
	}

	d.landingHolds.hold(landingHoldRebuildOM)
	if d.beginLandingPass() {
		t.Fatal("a pass started while a hold was on")
	}
	if got := d.landingPasses.Load(); got != 0 {
		t.Errorf("a refused pass left %d in flight, want 0", got)
	}
}
