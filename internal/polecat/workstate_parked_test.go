package polecat

import "testing"

func TestWorkstateDispositionWithParked(t *testing.T) {
	t.Parallel()

	t.Run("reusable becomes parked", func(t *testing.T) {
		t.Parallel()
		base := DecideWorkstate(WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean})
		if !base.Reusable {
			t.Fatalf("control disposition not reusable: %+v", base)
		}
		got := base.WithParked("parked (operator)")
		if got.Reusable || got.Reason != WorkstateReasonParked || got.ReuseStatus != WorkstateReuseStatusParked {
			t.Fatalf("parked disposition = %+v", got)
		}
		if len(got.Blockers) != 1 || got.Blockers[0] != "parked (operator)" {
			t.Fatalf("blockers = %v", got.Blockers)
		}
		// A park gates reuse only: nuke safety and recovery are decided elsewhere.
		if !got.SafeToNuke || got.NeedsRecovery || got.CountsTowardCapacity {
			t.Fatalf("park changed more than reuse: %+v", got)
		}
		if base.Reason != "reusable" || len(base.Blockers) != 0 {
			t.Fatalf("WithParked mutated its receiver: %+v", base)
		}
	})

	t.Run("non-reusable keeps its own reason", func(t *testing.T) {
		t.Parallel()
		base := DecideWorkstate(WorkstateInput{State: StateIdle, CleanupStatus: CleanupUncommitted})
		if base.Reusable {
			t.Fatalf("control disposition reusable: %+v", base)
		}
		got := base.WithParked("parked (operator)")
		if got.Reason != base.Reason || got.ReuseStatus != base.ReuseStatus || len(got.Blockers) != len(base.Blockers) {
			t.Fatalf("park overwrote a recovery verdict: %+v -> %+v", base, got)
		}
	})
}
