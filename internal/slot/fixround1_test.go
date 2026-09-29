package slot

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
)

// TestYield_MainBranchTestIsNotYieldedTo (M3): the daemon's main-branch test
// holds a gate-reserved slot but is not on the merge path, so crew do not
// yield to it; they still yield to a refinery holding the other reserved slot.
func TestYield_MainBranchTestIsNotYieldedTo(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	pool.MaxGateYield = 4 * tg.pollInterval

	mbt := tg.mustAcquirePool(t, town, "gastown/main-branch-test", pool)
	defer release(t, mbt)
	if mbt.Index != 0 {
		t.Fatalf("main-branch-test got slot %d, want reserved slot 0", mbt.Index)
	}

	crew, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	if err != nil {
		t.Fatal(err)
	}
	if elapsed != 0 {
		t.Fatalf("crew waited %s behind the main-branch test", elapsed)
	}
	release(t, crew)
	if rep, _ := tg.StatusPoolLocksOnly(town, pool); rep.YieldingToGate {
		t.Fatalf("status reports yielding to the main-branch test: holder=%+v", rep.GateHolder)
	}

	ref := tg.mustAcquirePool(t, town, "gastown/refinery", pool)
	defer release(t, ref)
	crew, err, elapsed = tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/crew/sloan", time.Hour, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, crew)
	if elapsed != pool.MaxGateYield {
		t.Fatalf("crew waited %s beside a refinery gate, want the yield cap %s", elapsed, pool.MaxGateYield)
	}
	if rep, _ := tg.StatusPoolLocksOnly(town, pool); !rep.YieldingToGate || rep.GateHolder == nil || rep.GateHolder.Role != "gastown/refinery" {
		t.Fatalf("status with a refinery gate: yielding=%v holder=%+v", rep.YieldingToGate, rep.GateHolder)
	}
}

// TestYield_BatchGateIsYieldedTo (M3): the batch gate is the refinery's own
// merge path, so it counts like the refinery.
func TestYield_BatchGateIsYieldedTo(t *testing.T) {
	t.Parallel()
	for role, want := range map[string]bool{
		"gastown/refinery": true, "hm/refinery-batch": true,
		"gastown/main-branch-test": false, "gastown/crew/sloan": false, "gastown/refinery-impostor-polecat": false,
	} {
		if got := IsMergeGateRole(role); got != want {
			t.Errorf("IsMergeGateRole(%q) = %v, want %v", role, got, want)
		}
	}
}

// TestIntent_ExpiryIgnoresAFarExpiresAt (M5): an intent is expired at
// RegisteredAt + GateIntentTTL even when its file claims a later ExpiresAt.
func TestIntent_ExpiryIgnoresAFarExpiresAt(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	writeIntent(t, town, GateIntent{Role: "gastown/refinery", Ref: "x", RegisteredAt: tg.clk.Now(), ExpiresAt: tg.clk.Now().Add(10 * time.Hour)})

	if _, ok := tg.pendingGate(town); !ok {
		t.Fatal("fresh intent not pending")
	}
	tg.clk.Advance(GateIntentTTL)
	if in, ok := tg.pendingGate(town); ok {
		t.Fatalf("intent past RegisteredAt+TTL still pending: %+v", in)
	}
}

// TestIntent_FutureRegisteredAtIsExpired (M5): an intent stamped in the
// future (a skewed or hand-written file) never holds anyone up.
func TestIntent_FutureRegisteredAtIsExpired(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	future := tg.clk.Now().Add(time.Hour)
	writeIntent(t, town, GateIntent{Role: "gastown/refinery", Ref: "x", RegisteredAt: future, ExpiresAt: future.Add(time.Minute)})
	if in, ok := tg.pendingGate(town); ok {
		t.Fatalf("intent registered in the future is pending: %+v", in)
	}
}

// TestIntent_EarlierExpiresAtWins (M5): an ExpiresAt before RegisteredAt+TTL
// still ends the intent.
func TestIntent_EarlierExpiresAtWins(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	writeIntent(t, town, GateIntent{Role: "gastown/refinery", Ref: "x", RegisteredAt: tg.clk.Now(), ExpiresAt: tg.clk.Now().Add(time.Minute)})
	tg.clk.Advance(time.Minute)
	if in, ok := tg.pendingGate(town); ok {
		t.Fatalf("intent past its own ExpiresAt still pending: %+v", in)
	}
}

// TestYield_DefaultCapIsTheConfigDefault (M6): the 30m default is defined once.
func TestYield_DefaultCapIsTheConfigDefault(t *testing.T) {
	t.Parallel()
	if DefaultMaxGateYield != config.DefaultContainerGateMaxGateYield {
		t.Fatalf("slot default %s != config default %s", DefaultMaxGateYield, config.DefaultContainerGateMaxGateYield)
	}
}

// TestYield_RunningGateReadsOwnerNotFlock (M4): the yield probe identifies a
// running gate from its owner file and its pid's liveness, without taking
// the slot's flock, so a waiter's probe can never make a gate's own
// FlockTryAcquire on that slot fail and push it onto a shared slot.
func TestYield_RunningGateReadsOwnerNotFlock(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()

	// Owner file for reserved slot 1 naming a live refinery, and no flock
	// file at all: a flock probe would find nothing.
	if err := atomicfile.EnsureDirAndWriteJSON(SlotOwnerPath(town, 1), Owner{Role: "gastown/refinery", PID: tg.pid, AcquiredAt: tg.clk.Now(), Slot: 1}); err != nil {
		t.Fatal(err)
	}
	owner, ok := tg.runningGate(town, pool)
	if !ok || owner == nil || owner.Role != "gastown/refinery" || owner.Slot != 1 {
		t.Fatalf("runningGate = %+v, %v; want the refinery on slot 1 from its owner file", owner, ok)
	}

	// The same owner file whose pid is gone is a dead gate.
	tg.gone[tg.pid] = true
	if owner, ok := tg.runningGate(town, pool); ok {
		t.Fatalf("runningGate reported a gate whose pid is gone: %+v", owner)
	}
}

func writeIntent(t *testing.T, town string, in GateIntent) {
	t.Helper()
	if err := atomicfile.EnsureDirAndWriteJSON(gateIntentPath(town, "gastown"), in); err != nil {
		t.Fatal(err)
	}
}
