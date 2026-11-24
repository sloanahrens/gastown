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
