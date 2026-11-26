package daemon

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// doctorRecorderDaemon is a daemon whose doctor_dog patrol is on and whose
// probes are the package's healthy fakes: no Dolt server is dialled, no
// backup root and no HOME are read.
func doctorRecorderDaemon(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			DoctorDog: &DoctorDogConfig{Enabled: true},
		}},
		doctorDogProbesFn: healthyDoctorProbes,
	}
}

// TestPatrolLastRunRecordersDoctorDog pins gt-nhz95j: a doctor_dog tick that
// takes the Dolt-task lock records its run in patrol_last_run.json, so gt jobs
// reads it as run-at-last instead of never_run; a tick skipped because a gc
// holds the lock records none.
func TestPatrolLastRunRecordersDoctorDog(t *testing.T) {
	t.Parallel()

	t.Run("a tick that takes the Dolt-task lock records a run", func(t *testing.T) {
		t.Parallel()
		d := doctorRecorderDaemon(t)

		before := time.Now()
		d.runDoctorDog()
		after := time.Now()

		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		got, ok := runs["doctor_dog"]
		if !ok {
			t.Fatal("doctor_dog recorded no run; gt jobs reads it as never_run")
		}
		if got.Before(before) || got.After(after) {
			t.Errorf("doctor_dog last run = %v, want a time within the tick (%v..%v)", got, before, after)
		}
	})

	t.Run("a tick skipped for a gc records no run", func(t *testing.T) {
		t.Parallel()
		d := doctorRecorderDaemon(t)
		var logs bytes.Buffer
		d.logger = log.New(&logs, "", 0)

		d.doltMaintMu.Lock()
		d.runDoctorDog()
		d.doltMaintMu.Unlock()

		if !strings.Contains(logs.String(), "doctor_dog: skipped: gc in flight") {
			t.Fatalf("the tick was not skipped for the gc, so this row proves nothing: %q", logs.String())
		}
		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		if got, ok := runs["doctor_dog"]; ok {
			t.Errorf("a gc-skipped tick recorded a run at %v; the tick made no run", got)
		}
	})
}

// overseerRecorderDaemon is a daemon on the package's fixture town whose
// overseer_report patrol is as given, on the given fake clock, with the
// fixture's report sources: no live store, seat or health read is made.
func overseerRecorderDaemon(t *testing.T, enabled bool, clock clockwork.Clock) *Daemon {
	t.Helper()
	w := newOverseerWorld(t, "2026-10-14T20:15:00Z")
	return &Daemon{
		config: &Config{TownRoot: w.town},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OverseerReport: &OverseerReportConfig{Enabled: enabled},
		}},
		overseerReportSeams: w.seams(),
		clock:               clock,
	}
}

// TestPatrolLastRunRecordersOverseerReportAndHMSync pins gt-x5dwpe: one run of
// each patrol leaves its entry in patrol_last_run.json, so gt jobs reads the
// row as run-at-last instead of never_run; a disabled patrol records none.
func TestPatrolLastRunRecordersOverseerReportAndHMSync(t *testing.T) {
	t.Parallel()

	t.Run("an overseer_report run records a run", func(t *testing.T) {
		t.Parallel()
		clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 26, 6, 45, 0, 0, time.UTC))
		d := overseerRecorderDaemon(t, true, clock)

		d.runOverseerReport()
		want := clock.Now()

		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		got, ok := runs["overseer_report"]
		if !ok {
			t.Fatal("overseer_report recorded no run; gt jobs reads it as never_run")
		}
		if !got.Equal(want) {
			t.Errorf("overseer_report last run = %v, want the fake clock's instant %v", got, want)
		}
	})

	t.Run("a disabled overseer_report records no run", func(t *testing.T) {
		t.Parallel()
		clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 26, 6, 45, 0, 0, time.UTC))
		d := overseerRecorderDaemon(t, false, clock)

		d.runOverseerReport()

		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		if got, ok := runs["overseer_report"]; ok {
			t.Errorf("a disabled patrol recorded a run at %v; it made no run", got)
		}
	})

	t.Run("an hm_sync pass records a run", func(t *testing.T) {
		t.Parallel()
		d, _, f := hmSyncTestDaemon(t, hmSyncConfig(true, "", ""))

		d.runHMSync()

		if len(f.calls) != 1 {
			t.Fatalf("ran %v, want one sync before the record proves anything", f.calls)
		}
		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		got, ok := runs["hm_sync"]
		if !ok {
			t.Fatal("hm_sync recorded no run; gt jobs reads it as never_run")
		}
		if !got.Equal(hmSyncNow) {
			t.Errorf("hm_sync last run = %v, want the pass's completion on its clock (%v)", got, hmSyncNow)
		}
	})

	t.Run("a disabled hm_sync records no run", func(t *testing.T) {
		t.Parallel()
		d, _, f := hmSyncTestDaemon(t, hmSyncConfig(false, "", ""))

		d.runHMSync()

		if len(f.calls) != 0 {
			t.Fatalf("ran %v with the patrol off, so this row proves nothing", f.calls)
		}
		runs, err := ReadPatrolLastRuns(d.config.TownRoot)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadPatrolLastRuns: %v", err)
		}
		if got, ok := runs["hm_sync"]; ok {
			t.Errorf("a disabled patrol recorded a run at %v; it made no run", got)
		}
	})
}
