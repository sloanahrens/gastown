package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// gt-ifijm: `gt scheduler run` is reached from the daemon heartbeat, the
// witness on SLOT_OPEN, and by hand; dispatchScheduledWork is the one place
// all three pass through, so the operator hold is enforced there.

// With the hold in place nothing is read or slung: the rig store's scan
// would fail (setupSchedulerScanFailureTown), so reaching the planner at all
// surfaces as an error.
func TestDispatchScheduledWork_OperatorHold_DispatchesNothing(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	townRoot := setupSchedulerScanFailureTown(t)
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

// Without the hold the same town proceeds into planning (and fails on the
// broken scan), proving the gate is what stopped the held run.
func TestDispatchScheduledWork_NoHold_Proceeds(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	townRoot := setupSchedulerScanFailureTown(t)

	_, err := dispatchScheduledWork(townRoot, "test", 1, false)
	if err == nil || !strings.Contains(err.Error(), "planning dispatch") {
		t.Fatalf("err = %v, want the planner's scan failure (dispatch proceeded past the gate)", err)
	}
}

func TestDropRigHeldBeads_RigEstopRemovesOnlyThatRig(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
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
