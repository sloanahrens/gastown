package daemon

import (
	"errors"
	"log"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTriggerDoltBackup_SkipsWhenNotDue is the regression test for crew
// review point 1 on gt-gxpwc: dolt_backup was the one patrol the first
// rework pass left with no persisted last-run or startup catch-up at all
// (daemon.go's dolt_backup ticker called syncDoltBackups directly, resetting
// on every restart exactly like jsonl_git_backup/wisp_reaper did before
// gt-ima2/gt-gxpwc). With a recent last-run record on disk, triggerDoltBackup
// must decline to start a cycle rather than firing on every tick or restart.
func TestTriggerDoltBackup_SkipsWhenNotDue(t *testing.T) {
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "dolt_backup", time.Now()); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf strings.Builder
	d := &Daemon{
		logger: log.New(&buf, "", 0),
		config: &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				DoltBackup: &DoltBackupConfig{Enabled: true},
			},
		},
	}

	d.triggerDoltBackup()
	if d.doltBackupRunning.Load() {
		t.Error("a declined trigger must not set the running guard")
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Errorf("expected a not-due log line, got: %q", buf.String())
	}
}

// TestTriggerDoltBackup_OverdueRunsAndPersists is the positive half of the
// gt-gxpwc rework's highest-severity finding: dolt_backup previously had no
// persisted last-run or startup catch-up whatsoever (daemon.go called
// syncDoltBackups straight off a restart-reset ticker), so it was starved by
// the exact gt-ima2 shape the other five patrols in this bug were already
// fixed for. This exercises the real triggerDoltBackup path end to end: with
// no last-run record (overdue), it must start a cycle, and that cycle must
// persist a last-run so an immediate re-check declines rather than re-running
// on every restart.
//
// dogPourBdFn stands in for the real `bd` subprocess pourDogMolecule would
// otherwise shell out to — the same seam TestDoltBackupSkipsWhileGCHoldsLock
// uses — so this stays a fast, hermetic unit test with no live Dolt
// connection. The temp town root has no .dolt-data directory, so the cycle
// reaches its own "data dir does not exist, skipping" early-out; what matters
// here is that the last-run defer registered before that early-out still
// fires.
func TestTriggerDoltBackup_OverdueRunsAndPersists(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("dolt_backup runs only on darwin")
	}

	townRoot := t.TempDir()
	var buf strings.Builder
	d := &Daemon{
		logger: log.New(&buf, "", 0),
		config: &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				DoltBackup: &DoltBackupConfig{Enabled: true},
			},
		},
		dogPourBdFn: func(args ...string) (string, error) {
			return "", errors.New("fake bd: unavailable")
		},
	}

	if _, found, _ := loadPatrolLastRun(townRoot, "dolt_backup"); found {
		t.Fatal("test setup: last-run should not exist yet")
	}

	d.triggerDoltBackup()

	deadline := time.Now().Add(2 * time.Second)
	for d.doltBackupRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for dolt_backup cycle to finish")
		}
		time.Sleep(time.Millisecond)
	}

	if _, found, err := loadPatrolLastRun(townRoot, "dolt_backup"); err != nil {
		t.Fatalf("loadPatrolLastRun: %v", err)
	} else if !found {
		t.Fatal("expected a persisted last-run after the cycle attempted its work")
	}

	buf.Reset()
	d.triggerDoltBackup()
	if d.doltBackupRunning.Load() {
		t.Error("expected the trigger to decline immediately after a completed cycle")
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Errorf("expected a not-due log line on the immediate re-check:\n%s", buf.String())
	}
}
