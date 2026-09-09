package refinery

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestRecordConflictTaskOnMR_ClearsStalePreVerified covers gt-nao: previously,
// recording a conflict-resolution task on an MR wisp left any prior
// pre_verified/pre_verified_at/pre_verified_base fields untouched even though
// the conflict means the target has diverged and the old verification no
// longer applies. The fields should be cleared so a later fast-path never
// skips gates against a stale base.
func TestRecordConflictTaskOnMR_ClearsStalePreVerified(t *testing.T) {
	workDir := t.TempDir()

	mrIssue := prepushMRIssue("gt-mr-1", "polecat/nux/gt-real", "main", "gt-real")
	mrIssue.Description += "\npre_verified: true\npre_verified_at: 2026-01-01T00:00:00Z\npre_verified_base: deadbeef"

	store := newPrepushStore(mrIssue)
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	e.beads = beads.NewWithStore(workDir, store)

	mr := &MRInfo{ID: "gt-mr-1", Branch: "polecat/nux/gt-real", Target: "main"}
	if err := e.recordConflictTaskOnMR(mr, "gt-task-1", 1, "cafebabe"); err != nil {
		t.Fatalf("recordConflictTaskOnMR: %v", err)
	}

	updated := store.issues["gt-mr-1"]
	got := beads.ParseMRFields(&beads.Issue{Description: updated.Description})
	if got == nil {
		t.Fatalf("expected parsable MR fields, got none in description: %q", updated.Description)
	}
	if got.ConflictTaskID != "gt-task-1" {
		t.Errorf("ConflictTaskID = %q, want gt-task-1", got.ConflictTaskID)
	}
	if got.LastConflictSHA != "cafebabe" {
		t.Errorf("LastConflictSHA = %q, want cafebabe", got.LastConflictSHA)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", got.RetryCount)
	}
	if got.PreVerified {
		t.Error("expected pre_verified to be cleared after a conflict is recorded")
	}
	if got.PreVerifiedAt != "" {
		t.Errorf("expected pre_verified_at cleared, got %q", got.PreVerifiedAt)
	}
	if got.PreVerifiedBase != "" {
		t.Errorf("expected pre_verified_base cleared, got %q", got.PreVerifiedBase)
	}
}

// TestRecordConflict_DeferredWhenSlotBusy covers the CLI-facing RecordConflict
// wrapper's non-error "deferred" path: when the merge slot is held by another
// resolution in progress, no conflict task is created and no error is
// returned — the MR simply retries next cycle.
func TestRecordConflict_DeferredWhenSlotBusy(t *testing.T) {
	workDir := t.TempDir()

	mrIssue := prepushMRIssue("gt-mr-2", "polecat/nux/gt-real2", "main", "gt-real2")
	store := newPrepushStore(mrIssue)
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	e.beads = beads.NewWithStore(workDir, store)
	e.mergeSlotEnsureExists = func() (string, error) { return "test-slot", nil }
	e.mergeSlotAcquire = func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error) {
		return &beads.MergeSlotStatus{Available: false, Holder: "someone-else/refinery"}, nil
	}
	e.mergeSlotRelease = func(holder string) error { return nil }

	taskID, err := e.RecordConflict("gt-mr-2")
	if err != nil {
		t.Fatalf("RecordConflict: unexpected error: %v", err)
	}
	if taskID != "" {
		t.Errorf("expected no task created while slot is busy, got %q", taskID)
	}

	// MR bead itself must be untouched — no conflict fields recorded while deferred.
	got := beads.ParseMRFields(&beads.Issue{Description: store.issues["gt-mr-2"].Description})
	if got != nil && got.ConflictTaskID != "" {
		t.Errorf("expected no conflict_task_id recorded while deferred, got %q", got.ConflictTaskID)
	}
}
