package runnerroll

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// backingTarget is the fake target with a Backer: it records the call and the
// directory it was given, and returns the configured names or error.
type backingTarget struct {
	*recordingTarget
	dir       string
	files     []string
	backupErr error
}

func (b *backingTarget) Backup(_ context.Context, dir string) ([]string, error) {
	b.log.add("backup")
	b.dir = dir
	return b.files, b.backupErr
}

// TestRollBackupDirIsStableForAGivenClock: the same reading names the same
// directory, whichever zone it is expressed in, and the directory sits under
// the town root the roll was given.
func TestRollBackupDirIsStableForAGivenClock(t *testing.T) {
	t.Parallel()
	const want = "/town/.runtime/runner-roll/20261119T120000Z"
	if got := BackupDir("/town", fixedNow); got != want {
		t.Errorf("BackupDir = %q, want %q", got, want)
	}
	if got := BackupDir("/town", fixedNow); got != want {
		t.Errorf("BackupDir at the same clock = %q, want %q", got, want)
	}
	if got := BackupDir("/town/", fixedNow); got != want {
		t.Errorf("BackupDir with a trailing separator = %q, want %q", got, want)
	}
	west := time.FixedZone("UTC-1", -3600)
	if got := BackupDir("/town", fixedNow.In(west)); got != want {
		t.Errorf("BackupDir of the same instant in another zone = %q, want %q", got, want)
	}
}

// TestRollBackupRunsBeforeApplyAndPrintsTheDirAndFiles: a target that backs up
// is backed up after the runs wait and before the apply, into BackupDir, and
// the roll prints the directory and each name the target wrote.
func TestRollBackupRunsBeforeApplyAndPrintsTheDirAndFiles(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.TownRoot = "/town"
	b := &backingTarget{
		recordingTarget: f.target,
		files:           []string{"deployment.yaml", "configmap.yaml"},
	}

	if err := Run(context.Background(), f.deps, b, f.opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{"hold:take", "landing", "runs", "backup", "apply", "ready", "hold:release"}
	if !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v: the backup runs after the waits and before the apply", f.log.calls, want)
	}
	if wantDir := BackupDir("/town", fixedNow); b.dir != wantDir {
		t.Errorf("backup dir = %q, want %q", b.dir, wantDir)
	}
	out := f.out.String()
	if line := "backup: " + BackupDir("/town", fixedNow); !strings.Contains(out, line) {
		t.Errorf("output lacks %q; got:\n%s", line, out)
	}
	for _, file := range b.files {
		if !strings.Contains(out, file) {
			t.Errorf("output lacks the backed-up file %q; got:\n%s", file, out)
		}
	}
	if f.store.present {
		t.Error("the landing hold was not released")
	}
}

// TestRollBackupFailureStopsTheRollBeforeApply: a backup that fails is the
// roll's error, names the target and its directory, and leaves the live target
// untouched while the hold is released as usual.
func TestRollBackupFailureStopsTheRollBeforeApply(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.TownRoot = "/town"
	backupErr := errors.New("the live Deployment could not be read")
	b := &backingTarget{
		recordingTarget: f.target,
		files:           []string{"deployment.yaml"},
		backupErr:       backupErr,
	}

	err := Run(context.Background(), f.deps, b, f.opts)

	if !errors.Is(err, backupErr) {
		t.Fatalf("error = %v, want it to wrap %v", err, backupErr)
	}
	if want := "backing up the runner to " + BackupDir("/town", fixedNow); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name the target and the directory (%q)", err, want)
	}
	want := []string{"hold:take", "landing", "runs", "backup", "hold:release"}
	if !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v: nothing is applied after a failed backup", f.log.calls, want)
	}
	if f.store.present {
		t.Error("the landing hold was not released after a failed backup")
	}
	if out := f.out.String(); strings.Contains(out, "backup: ") {
		t.Errorf("output claims a backup that failed; got:\n%s", out)
	}
}

// TestRollBackupTargetWithoutBackerIsUnchanged: a target without Backup runs
// the steps it always did, prints no backup line, and needs no town root.
func TestRollBackupTargetWithoutBackerIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newRollFixture()

	if err := Run(context.Background(), f.deps, f.target, f.opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !slices.Equal(f.log.calls, rollStepOrder) {
		t.Errorf("calls = %v, want %v", f.log.calls, rollStepOrder)
	}
	if slices.Contains(f.log.calls, "backup") {
		t.Errorf("calls = %v: a target without Backup was backed up", f.log.calls)
	}
	if out := f.out.String(); strings.Contains(out, "backup: ") {
		t.Errorf("output claims a backup for a target without Backup; got:\n%s", out)
	}
}

// TestRollBackupRefusesABackerTargetWithoutATownRoot: a target that backs up
// needs a town root to write under, and the roll refuses before it takes the
// hold rather than writing a backup at a relative path.
func TestRollBackupRefusesABackerTargetWithoutATownRoot(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	b := &backingTarget{recordingTarget: f.target, files: []string{"deployment.yaml"}}

	err := Run(context.Background(), f.deps, b, f.opts)

	if err == nil || !strings.Contains(err.Error(), "no town root") {
		t.Fatalf("error = %v, want it to name the missing town root", err)
	}
	if len(f.log.calls) != 0 {
		t.Errorf("calls = %v, want none: a refused roll takes nothing", f.log.calls)
	}
	if f.store.present {
		t.Error("a refused roll wrote the landing hold")
	}
}

// TestRollBackupDryRunNeverBacksUp: a dry run applies nothing, so it backs up
// nothing and needs no town root to back up under.
func TestRollBackupDryRunNeverBacksUp(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.opts.DryRun = true
	b := &backingTarget{
		recordingTarget: f.target,
		backupErr:       errors.New("a dry run must not back up"),
	}

	if err := Run(context.Background(), f.deps, b, f.opts); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if want := []string{"diff"}; !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v: a dry run backs up nothing", f.log.calls, want)
	}
	if out := f.out.String(); strings.Contains(out, "backup: ") {
		t.Errorf("dry run output claims a backup; got:\n%s", out)
	}
}
