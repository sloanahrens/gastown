package slot

import (
	"testing"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
)

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

	// Owner file for reserved slot 1 naming a live landing worker, and no flock
	// file at all: a flock probe would find nothing.
	if err := atomicfile.EnsureDirAndWriteJSON(SlotOwnerPath(town, 1), Owner{Role: "gastown/landing", PID: tg.pid, AcquiredAt: tg.clk.Now(), Slot: 1}); err != nil {
		t.Fatal(err)
	}
	owner, ok := tg.runningGate(town, pool)
	if !ok || owner == nil || owner.Role != "gastown/landing" || owner.Slot != 1 {
		t.Fatalf("runningGate = %+v, %v; want the landing worker on slot 1 from its owner file", owner, ok)
	}

	// The same owner file whose pid is gone is a dead gate.
	tg.gone[tg.pid] = true
	if owner, ok := tg.runningGate(town, pool); ok {
		t.Fatalf("runningGate reported a gate whose pid is gone: %+v", owner)
	}
}
