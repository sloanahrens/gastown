package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// gt-ifijm: `gt scheduler run` is reached from the daemon heartbeat, the
// witness on SLOT_OPEN, and by hand; dispatchScheduledWork is the one place
// all three pass through, so the operator hold is enforced there.

// With the hold in place nothing is read or slung: the dispatch lock is never
// taken, so the gate stops the run before the planner. (The hermetic harness
// scrubs GT_*, so GT_SEAT_REFILL_HOLD cannot move the hold file.)
func TestDispatchScheduledWork_OperatorHold_DispatchesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	n, err := dispatchScheduledWork(townRoot, "test", 1, false)
	if err != nil || n != 0 {
		t.Fatalf("dispatchScheduledWork under a hold = (%d, %v), want (0, nil) without planning", n, err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".runtime", "scheduler-dispatch.lock")); err == nil {
		t.Error("the dispatch lock was taken during a hold: the gate must come first")
	}
}

func TestDropRigHeldBeads_RigEstopRemovesOnlyThatRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	plan := capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{
			{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"},
			{ID: "ctx-2", WorkBeadID: "om-b", TargetRig: "om"},
		},
		Skipped: 1,
	}

	got := dropRigHeldBeads(townRoot, plan)

	if len(got.ToDispatch) != 1 || got.ToDispatch[0].WorkBeadID != "om-b" {
		t.Errorf("ToDispatch = %+v, want only om-b", got.ToDispatch)
	}
	if got.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (held bead counted as skipped, not failed)", got.Skipped)
	}
}
