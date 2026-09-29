package slot

import (
	"strings"
	"testing"
	"time"
)

// TestIntent_PendingGateMakesNonGateYield is the controller's live finding on
// gt-22hdp.29: the refinery checks host load BEFORE it takes a slot, so under
// crew load the gate never holds one and nothing yields. A refinery that has
// picked a ready MR registers intent; new non-gate starts wait on it exactly
// as on a held gate slot, and start once it is cleared.
func TestIntent_PendingGateMakesNonGateYield(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	if err := tg.RegisterGateIntent(town, "gastown", "gt-wisp-rpf"); err != nil {
		t.Fatal(err)
	}

	done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	for i := 0; i < 3; i++ {
		waitBlocked(t, tg.clk)
		select {
		case got := <-done:
			t.Fatalf("crew acquire returned while a gate was pending: h=%+v err=%v", got.h, got.err)
		default:
		}
		tg.clk.Advance(tg.pollInterval)
	}
	if out := tg.probe.String(); !strings.Contains(out, "gate pending") || !strings.Contains(out, "gt-wisp-rpf") {
		t.Errorf("wait line = %q, want the pending gate and its MR named", out)
	}

	if err := tg.ClearGateIntent(town, "gastown"); err != nil {
		t.Fatal(err)
	}
	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("crew acquire after the intent cleared: %v", got.err)
	}
	defer release(t, got.h)

	hist, err := History(town)
	if err != nil {
		t.Fatal(err)
	}
	if last := hist[len(hist)-1]; last.Reason != WaitReasonGatePending || last.HolderRole != "gastown/refinery" {
		t.Fatalf("history entry = %+v, want the wait attributed to the pending gate", last)
	}
}

// TestIntent_ExpiresAfterTTL: an intent nobody refreshes or clears (the
// refinery died after picking an MR) stops holding anyone up once it expires.
func TestIntent_ExpiresAfterTTL(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	if err := tg.RegisterGateIntent(town, "gastown", "gt-wisp-rpf"); err != nil {
		t.Fatal(err)
	}
	if rep, _ := tg.StatusPoolLocksOnly(town, pool); !rep.YieldingToGate || rep.GatePending == nil {
		t.Fatalf("fresh intent not reported: %+v", rep)
	}

	tg.clk.Advance(GateIntentTTL)
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 {
		t.Fatalf("crew waited %s on an expired intent", elapsed)
	}
	if rep, _ := tg.StatusPoolLocksOnly(town, pool); rep.YieldingToGate || rep.GatePending != nil {
		t.Fatalf("expired intent still reported: %+v", rep)
	}
}

// TestIntent_RefreshExtends: re-registering (the refinery's next gt mq next
// with the queue still non-empty) restarts the TTL.
func TestIntent_RefreshExtends(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	if err := tg.RegisterGateIntent(town, "gastown", "a"); err != nil {
		t.Fatal(err)
	}
	tg.clk.Advance(GateIntentTTL - time.Minute)
	if err := tg.RegisterGateIntent(town, "gastown", "b"); err != nil {
		t.Fatal(err)
	}
	tg.clk.Advance(2 * time.Minute)
	rep, err := tg.StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GatePending == nil || rep.GatePending.Ref != "b" {
		t.Fatalf("refreshed intent = %+v, want MR b still pending", rep.GatePending)
	}
}

// TestIntent_GateAndKnobOffDoNotYield: a pending gate never delays a gate, and
// with YieldToGate off nothing yields to it.
func TestIntent_GateAndKnobOffDoNotYield(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	if err := tg.RegisterGateIntent(town, "gastown", "gt-wisp-rpf"); err != nil {
		t.Fatal(err)
	}

	g, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "hm/refinery", time.Minute, pool) })
	if err != nil || elapsed != 0 {
		t.Fatalf("gate beside a pending gate: err=%v waited %s", err, elapsed)
	}
	release(t, g)

	off := pool
	off.YieldToGate = false
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Minute, off) })
	if err != nil || elapsed != 0 {
		t.Fatalf("knob off: err=%v waited %s", err, elapsed)
	}
	release(t, h)
	if rep, _ := tg.StatusPoolLocksOnly(town, off); rep.YieldingToGate || rep.GatePending != nil {
		t.Fatalf("knob off still reports a yield: %+v", rep)
	}
}

// TestIntent_PendingYieldSharesTheCap: time spent yielding to a pending gate
// counts against MaxGateYield like time yielding to a running one, so a
// refinery that keeps re-registering cannot starve the town.
func TestIntent_PendingYieldSharesTheCap(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	pool.MaxGateYield = 6 * tg.pollInterval
	if err := tg.RegisterGateIntent(town, "gastown", "gt-wisp-rpf"); err != nil {
		t.Fatal(err)
	}

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != pool.MaxGateYield {
		t.Fatalf("crew waited %s, want the cap %s", elapsed, pool.MaxGateYield)
	}
}

// TestIntent_IntentFilesAreNotSlots: an intent file in the lock directory is
// never mistaken for a pool slot.
func TestIntent_IntentFilesAreNotSlots(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := tg.RegisterGateIntent(town, "gastown", "x"); err != nil {
		t.Fatal(err)
	}
	rep, err := tg.StatusPoolLocksOnly(town, yieldPool())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 4 {
		t.Fatalf("Total = %d with an intent registered, want 4", rep.Total)
	}
}

// TestIntent_ClearMissingIsNoError: clearing a rig with no intent is fine
// (gt mq next on an empty queue clears on every call).
func TestIntent_ClearMissingIsNoError(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	if err := tg.ClearGateIntent(t.TempDir(), "gastown"); err != nil {
		t.Fatalf("ClearGateIntent with nothing registered: %v", err)
	}
}
