package update

import (
	"context"
	"testing"
)

// TestRunRecordsTheRollbackOnTheState holds the run's record to the rollback its
// install reported: the run that rolled back records the check and the pair
// restored, and a run that installs clean records none, so an earlier rollback
// cannot outlive the run that made it.
func TestRunRecordsTheRollbackOnTheState(t *testing.T) {
	t.Parallel()
	restored := ImagePair{Daemon: "reg/gt@sha256:" + repeatHex('a'), Polecat: "reg/pc@sha256:" + repeatHex('b'), Commit: "1111111111111111111111111111111111111111"}
	rollback := &RollbackRecord{Check: CheckRegistryTag, Restored: restored}

	f := newRunFixture(t)
	f.inst.result = InstallResult{Event: "failed", Exit: 1, Reason: "restored", Rollback: rollback}
	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeFailed)
	}
	if final.Rollback == nil || *final.Rollback != *rollback {
		t.Fatalf("Rollback = %+v, want %+v", final.Rollback, rollback)
	}

	f.inst.result = InstallResult{Event: "installed", Exit: 0}
	clean, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if clean.Rollback != nil {
		t.Errorf("Rollback after a clean run = %+v, want none", clean.Rollback)
	}
}

// TestRunStoreWriteFailureOnTheRollbackIsReturned holds the write that records a
// rollback to the rule every state write follows: a publish that fails ends the
// run with the write error, never as a recorded rollback.
func TestRunStoreWriteFailureOnTheRollbackIsReturned(t *testing.T) {
	t.Parallel()
	rolling := func(t *testing.T) *runFixture {
		f := newRunFixture(t)
		f.inst.result = InstallResult{
			Event: "failed", Exit: 1, Reason: "restored",
			Rollback: &RollbackRecord{Check: CheckRollout, Restored: ImagePair{Daemon: "reg/gt@sha256:" + repeatHex('a'), Commit: "1111111111111111111111111111111111111111"}},
		}
		return f
	}
	total := publishCount(t, rolling(t))
	assertEveryPublishIsChecked(t, total, rolling)
}

// repeatHex is a 64-character lowercase hex digest made of one character, so a
// test pair is a well-formed digest without a real image.
func repeatHex(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
