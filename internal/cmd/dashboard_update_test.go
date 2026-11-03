package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// updateStatusTestReader is a reader over a record written by the update
// package itself, under a temp town root: read() takes its now, so a test
// never waits or reads the live town.
func updateStatusTestReader(t *testing.T, st update.State) *updateStatusReader {
	t.Helper()
	root := t.TempDir()
	store := &update.Store{Dir: update.Dir(root)}
	if err := store.Write(st); err != nil {
		t.Fatalf("writing the fixture record: %v", err)
	}
	return newUpdateStatusReader(root)
}

// writeUpdateStateFile puts raw bytes where the reader looks, for the records
// the store itself refuses to write.
func writeUpdateStateFile(t *testing.T, body string) *updateStatusReader {
	t.Helper()
	root := t.TempDir()
	dir := update.Dir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return newUpdateStatusReader(root)
}

// A run in flight gives the feed its live elapsed time, its heartbeat age, the
// step it is on and every step of the plan.
func TestUpdateStatusFromARunningState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	r := updateStatusTestReader(t, update.State{
		RunID:       "run-1",
		Source:      update.SourceManual,
		StartedAt:   now.Add(-8 * time.Minute),
		HeartbeatAt: now.Add(-20 * time.Second),
		Current:     update.StepInstall,
		Outcome:     update.OutcomeRunning,
		Steps: []update.StepRecord{
			{Name: update.StepPlan, WillRun: true, Result: update.ResultOK, Detail: "six steps"},
			{Name: update.StepDrain, WillRun: true, Result: update.ResultRunning},
			{Name: update.StepMaintenance, WillRun: false, Result: update.ResultSkipped, Detail: "no schedule"},
		},
	})
	got := r.read(now)
	if got == nil {
		t.Fatal("a running record read as no run")
	}
	if !got.Running {
		t.Errorf("Running = false for outcome %q", got.Outcome)
	}
	if got.RunID != "run-1" || got.Source != update.SourceManual || got.Outcome != string(update.OutcomeRunning) {
		t.Errorf("run = %s/%s/%s", got.RunID, got.Source, got.Outcome)
	}
	if got.Current != string(update.StepInstall) {
		t.Errorf("Current = %q, want %q", got.Current, update.StepInstall)
	}
	if got.ElapsedSec != 8*60 {
		t.Errorf("ElapsedSec = %d, want %d", got.ElapsedSec, 8*60)
	}
	if got.HeartbeatAgeSec != 20 {
		t.Errorf("HeartbeatAgeSec = %d, want 20", got.HeartbeatAgeSec)
	}
	if len(got.Steps) != 3 {
		t.Fatalf("Steps = %+v", got.Steps)
	}
	if s := got.Steps[0]; s.Name != "plan" || !s.WillRun || s.Result != "ok" || s.Detail != "six steps" {
		t.Errorf("step 0 = %+v", s)
	}
	if s := got.Steps[1]; s.Name != "drain" || s.Result != "running" || s.Detail != "" {
		t.Errorf("step 1 = %+v", s)
	}
	if s := got.Steps[2]; s.Name != "maintenance" || s.WillRun || s.Result != "skipped" || s.Detail != "no schedule" {
		t.Errorf("step 2 = %+v", s)
	}
}

// A run that ended is drawn from its own end, not from now, and one that ended
// more than a day ago is no longer the feed's business.
func TestUpdateStatusFromAFinishedState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-2 * time.Hour)
	r := updateStatusTestReader(t, update.State{
		RunID:       "run-2",
		Source:      update.SourceDaemon,
		StartedAt:   ended.Add(-5 * time.Minute),
		HeartbeatAt: ended.Add(-time.Minute),
		EndedAt:     ended,
		Current:     update.StepLift,
		Outcome:     update.OutcomeOK,
	})
	got := r.read(now)
	if got == nil {
		t.Fatal("a run that ended two hours ago read as no run")
	}
	if got.Running {
		t.Error("Running = true for a finished run")
	}
	if got.Outcome != string(update.OutcomeOK) {
		t.Errorf("Outcome = %q, want %q", got.Outcome, update.OutcomeOK)
	}
	if got.ElapsedSec != 5*60 {
		t.Errorf("ElapsedSec = %d, want %d (EndedAt minus StartedAt)", got.ElapsedSec, 5*60)
	}
	if got.HeartbeatAgeSec != 0 {
		t.Errorf("HeartbeatAgeSec = %d for a finished run, want 0", got.HeartbeatAgeSec)
	}
	if !got.EndedAt.Equal(ended) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, ended)
	}

	old := updateStatusTestReader(t, update.State{
		RunID:     "run-3",
		StartedAt: now.Add(-25*time.Hour - 5*time.Minute),
		EndedAt:   now.Add(-25 * time.Hour),
		Outcome:   update.OutcomeOK,
	})
	if got := old.read(now); got != nil {
		t.Errorf("a run that ended 25 hours ago read as %+v, want nil", got)
	}
}

// A town with no run, a record that is not JSON and a record of a version this
// build does not know are all no section, and none of them panics.
func TestUpdateStatusAbsentOrUnreadableIsNil(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	absent := newUpdateStatusReader(t.TempDir())
	if got := absent.read(now); got != nil {
		t.Errorf("a town with no record read as %+v, want nil", got)
	}
	malformed := writeUpdateStateFile(t, "this is not json")
	if got := malformed.read(now); got != nil {
		t.Errorf("a malformed record read as %+v, want nil", got)
	}
	future := writeUpdateStateFile(t, `{"v":2,"run_id":"run-4","outcome":"running"}`)
	if got := future.read(now); got != nil {
		t.Errorf("a version 2 record read as %+v, want nil", got)
	}
}
