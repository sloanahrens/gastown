package cmd

import (
	"fmt"
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
// step it is on and every step of the plan, and no end time.
func TestDashboardUpdateStatusFromARunningState(t *testing.T) {
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
	if got.EndedAt != nil {
		t.Errorf("EndedAt = %v for a running run, want nil", got.EndedAt)
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
func TestDashboardUpdateStatusFromAFinishedState(t *testing.T) {
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
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
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

// A record that still says running but has not beaten for ten minutes is a
// dead job, and the feed reports it as one rather than dropping it: Running
// stays true and HeartbeatAgeSec carries the age so the page can draw the dead
// state. Only a heartbeat more than a day old is history and reads as no run.
func TestDashboardUpdateStatusKeepsADeadRunningRun(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	state := func(beat time.Time) update.State {
		return update.State{
			RunID:       "run-1",
			StartedAt:   now.Add(-20 * time.Minute),
			HeartbeatAt: beat,
			Current:     update.StepInstall,
			Outcome:     update.OutcomeRunning,
		}
	}
	dead := updateStatusTestReader(t, state(now.Add(-10*time.Minute)))
	got := dead.read(now)
	if got == nil {
		t.Fatal("a running record with a ten-minute-old heartbeat read as no run")
	}
	if !got.Running {
		t.Error("Running = false for a dead running run, want true")
	}
	if got.HeartbeatAgeSec != 10*60 {
		t.Errorf("HeartbeatAgeSec = %d, want %d", got.HeartbeatAgeSec, 10*60)
	}
	if got.EndedAt != nil {
		t.Errorf("EndedAt = %v for a dead running run, want nil", got.EndedAt)
	}

	ancient := updateStatusTestReader(t, state(now.Add(-25*time.Hour)))
	if got := ancient.read(now); got != nil {
		t.Errorf("a running record whose heartbeat is 25 hours old read as %+v, want nil", got)
	}
}

// A town with no run, a record that is not JSON and a record of a version this
// build does not know are all no section, and none of them panics.
func TestDashboardUpdateStatusAbsentOrUnreadableIsNil(t *testing.T) {
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

// A record that cannot be read is reported, once per failure, and a town with
// no record is not: absence is a normal state, an unreadable record is not.
func TestDashboardUpdateStatusUnreadableLogsOneLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	absent := newUpdateStatusReader(t.TempDir()).withLogf(logf)
	if got := absent.read(now); got != nil {
		t.Fatalf("a town with no record read as %+v, want nil", got)
	}
	if len(logs) != 0 {
		t.Errorf("logs for a town with no record = %q, want none", logs)
	}

	malformed := writeUpdateStateFile(t, "this is not json").withLogf(logf)
	for i := 0; i < 2; i++ {
		if got := malformed.read(now); got != nil {
			t.Fatalf("read %d: a malformed record read as %+v, want nil", i+1, got)
		}
	}
	if len(logs) != 1 {
		t.Errorf("logs for an unreadable record read twice = %q, want one line", logs)
	}
}
