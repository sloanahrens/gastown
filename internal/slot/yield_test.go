package slot

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// yieldPool is the town's shape (operational.container_gate on 2026-09-29):
// four slots, two reserved for the gate, with non-gate starts yielding to a
// running gate (gt-22hdp.29).
func yieldPool() Pool {
	return Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true}
}

// TestYield_NonGateWaitsWhileGateHoldsReservedSlot: while the refinery holds a
// gate-reserved slot, a crew suite does not start even though both shared
// slots are free; it starts on the first pass after the gate releases.
func TestYield_NonGateWaitsWhileGateHoldsReservedSlot(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	if gate.Index != 0 {
		t.Fatalf("refinery got slot %d, want reserved slot 0", gate.Index)
	}

	done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	// Several blocked passes: the crew suite must still be waiting.
	for i := 0; i < 3; i++ {
		waitBlocked(t, tg.clk)
		select {
		case got := <-done:
			t.Fatalf("crew acquire returned while the gate held a reserved slot: h=%+v err=%v", got.h, got.err)
		default:
		}
		tg.clk.Advance(tg.pollInterval)
	}
	if out := tg.probe.String(); !strings.Contains(out, "gate running") || !strings.Contains(out, "gastown/refinery") {
		t.Errorf("wait line = %q, want it to say the gate is running and name it", out)
	}

	release(t, gate)
	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("crew acquire after the gate released: %v", got.err)
	}
	defer release(t, got.h)
	if got.h.Index != 2 {
		t.Fatalf("crew got slot %d, want shared slot 2", got.h.Index)
	}

	hist, err := History(town)
	if err != nil {
		t.Fatal(err)
	}
	last := hist[len(hist)-1]
	if last.Role != "gastown/crew/sloan" || last.Reason != WaitReasonGateRunning || last.HolderRole != "gastown/refinery" {
		t.Fatalf("history entry = %+v, want the crew wait attributed to the running gate", last)
	}
}

// TestYield_GateNeverWaitsOnNonGateHolders: crew suites holding every shared
// slot do not delay a gate, and a gate does not yield to another gate.
func TestYield_GateNeverWaitsOnNonGateHolders(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	a := tg.mustAcquirePool(t, town, "gastown/crew/sloan", pool)
	b := tg.mustAcquirePool(t, town, "gastown/amber", pool)
	defer release(t, a)
	defer release(t, b)

	for i, role := range []string{"gastown/refinery", "hm/refinery-batch"} {
		h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, role, time.Minute, pool) })
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		defer release(t, h)
		if elapsed != 0 || h.Index != i {
			t.Fatalf("%s got slot %d after %s, want reserved slot %d with no wait", role, h.Index, elapsed, i)
		}
	}
}

// TestYield_RunningHolderIsNotPreempted: a crew suite already holding a
// shared slot keeps it when a gate starts; only new starts wait.
func TestYield_RunningHolderIsNotPreempted(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	crew := tg.mustAcquirePool(t, town, "gastown/crew/sloan", pool)
	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)

	rep, err := StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Slots[crew.Index].Held || rep.Slots[crew.Index].Owner.Role != "gastown/crew/sloan" {
		t.Fatalf("crew slot %d after the gate started: %+v", crew.Index, rep.Slots[crew.Index])
	}
	release(t, crew)
}

// TestYield_KnobOffRestoresOldBehaviour: with YieldToGate false, a crew suite
// takes a free shared slot beside a running gate at once.
func TestYield_KnobOffRestoresOldBehaviour(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	pool.YieldToGate = false

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 || h.Index != 2 {
		t.Fatalf("crew got slot %d after %s, want slot 2 with no wait", h.Index, elapsed)
	}

	rep, err := StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.YieldingToGate {
		t.Fatalf("status reports yielding with the knob off: %+v", rep)
	}
}

// TestYield_NoReservedSlotsMeansNoYield: a pool with no
// reserved slots has nothing to yield to (the single-slot default).
func TestYield_NoReservedSlotsMeansNoYield(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 3, YieldToGate: true}

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 {
		t.Fatalf("crew waited %s in a pool with no reserved slots", elapsed)
	}
}

// TestYield_LeakedGateHoldIsIgnored: a reserved slot whose flock is still held
// but whose recorded owner process is gone (the descriptor outlived its holder
// in some orphan) is not a running gate, so nothing yields to it.
func TestYield_LeakedGateHoldIsIgnored(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)
	tg.gone[tg.pid] = true

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 {
		t.Fatalf("crew waited %s behind a gate hold whose owner is gone", elapsed)
	}

	rep, err := tg.StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.YieldingToGate {
		t.Fatalf("status reports yielding to a dead gate: %+v", rep)
	}
}

// TestYield_BoundedByMaxGateYield: a gate that never releases (a hung gate,
// or gates running back to back) delays a non-gate start by at most
// MaxGateYield; after that the waiter competes for a shared slot as before.
func TestYield_BoundedByMaxGateYield(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	pool.MaxGateYield = 10 * tg.pollInterval

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	if err != nil {
		t.Fatalf("crew acquire behind a gate that never releases: %v", err)
	}
	defer release(t, h)
	if elapsed != pool.MaxGateYield {
		t.Fatalf("crew waited %s, want exactly the yield cap %s", elapsed, pool.MaxGateYield)
	}
	if h.Index != 2 {
		t.Fatalf("crew got slot %d, want shared slot 2", h.Index)
	}
	if !strings.Contains(tg.probe.String(), "yield cap") {
		t.Errorf("probe output = %q, want the capped yield announced", tg.probe.String())
	}
}

// TestYield_DefaultCapApplies: MaxGateYield <= 0 means DefaultMaxGateYield.
func TestYield_DefaultCapApplies(t *testing.T) {
	t.Parallel()
	if got := (Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true}).normalized().MaxGateYield; got != DefaultMaxGateYield {
		t.Fatalf("normalized MaxGateYield = %s, want %s", got, DefaultMaxGateYield)
	}
	if DefaultMaxGateYield <= 0 {
		t.Fatalf("DefaultMaxGateYield = %s: a non-positive cap would disable the starvation bound", DefaultMaxGateYield)
	}
}

// TestYield_TimeoutWhileYieldingIsAttributed: a waiter that gives up while
// yielding says so, in the error path's history entry.
func TestYield_TimeoutWhileYieldingIsAttributed(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)

	if _, err, _ := tg.run(t, func() (*Handle, error) {
		return tg.AcquirePool(town, "gastown/crew/sloan", 3*tg.pollInterval, pool)
	}); err == nil {
		t.Fatal("crew acquire succeeded inside its timeout while the gate ran")
	}
	hist, err := History(town)
	if err != nil {
		t.Fatal(err)
	}
	last := hist[len(hist)-1]
	if !last.TimedOut || last.Reason != WaitReasonGateRunning {
		t.Fatalf("history entry = %+v, want a timeout attributed to the running gate", last)
	}
}

// TestYield_GateDescendantDoesNotYieldToItsOwnGate: work nested inside a gate
// hold (a process carrying a gate role's marker) that names a different role
// does not yield to the very gate it runs under — that would stall the gate
// on itself until the yield cap.
func TestYield_GateDescendantDoesNotYieldToItsOwnGate(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)

	child := tg.child()
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return child.AcquirePool(town, "gastown/crew/sloan", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 || h.reentrant {
		t.Fatalf("nested acquire under the gate: reentrant=%v waited %s, want a real shared slot with no wait", h.reentrant, elapsed)
	}
}

// TestYield_StatusShowsGateRunning: status names the running gate new
// non-gate suites are waiting on, and clears once it releases.
func TestYield_StatusShowsGateRunning(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	rep, err := tg.StatusPool(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.YieldingToGate || rep.GateHolder == nil || rep.GateHolder.Role != "gastown/refinery" || rep.GateHolder.Slot != 0 {
		t.Fatalf("status with a gate running: yielding=%v holder=%+v", rep.YieldingToGate, rep.GateHolder)
	}
	if rep.Busy() {
		t.Fatalf("a running gate alone must not read busy (gates may still start): %+v", rep)
	}

	release(t, gate)
	rep, err = tg.StatusPool(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.YieldingToGate || rep.GateHolder != nil {
		t.Fatalf("status after the gate released: yielding=%v holder=%+v", rep.YieldingToGate, rep.GateHolder)
	}
}

// TestPoolFromConfig: the town's operational.container_gate block maps onto a
// Pool, and an unset block yields to the gate by default.
func TestPoolFromConfig(t *testing.T) {
	t.Parallel()
	slots, reserved, off := 4, 2, false

	got := PoolFromConfig(&config.ContainerGateThresholds{Slots: &slots, ReservedForGate: &reserved})
	want := Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true, MaxGateYield: config.DefaultContainerGateMaxGateYield}
	if got != want {
		t.Fatalf("PoolFromConfig(defaults) = %+v, want %+v", got, want)
	}

	got = PoolFromConfig(&config.ContainerGateThresholds{Slots: &slots, ReservedForGate: &reserved, YieldToGate: &off, MaxGateYield: "5m"})
	want = Pool{Slots: 4, ReservedForGate: 2, YieldToGate: false, MaxGateYield: 5 * time.Minute}
	if got != want {
		t.Fatalf("PoolFromConfig(overrides) = %+v, want %+v", got, want)
	}

	if got := PoolFromConfig(nil); got != (Pool{Slots: 1, YieldToGate: true, MaxGateYield: config.DefaultContainerGateMaxGateYield}) {
		t.Fatalf("PoolFromConfig(nil) = %+v, want the single-slot default", got)
	}
}

// TestYield_StaleGateMarkerStillYields: a gate's marker outlives its hold in
// every process spawned while it held (the daemon's agents, gt-off9). Such a
// process is not nested under the gate that is running now, so it yields to it
// like anyone else.
func TestYield_StaleGateMarkerStillYields(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	pool.MaxGateYield = 4 * tg.pollInterval

	// Inherited from a main-branch-test hold on slot 1 that has since ended.
	child := tg.child()
	child.env.Setenv(ReentrantEnvVar, reentrantEnvValue(town, 1, "gastown/main-branch-test", foreignPID()+7))

	gate := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, gate)

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return child.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != pool.MaxGateYield {
		t.Fatalf("stale-marker acquire waited %s, want it to yield to the running gate for the cap %s", elapsed, pool.MaxGateYield)
	}
}
