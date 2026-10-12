package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/templates"
)

// respawnCheck is a check whose supervisor reports st, canned: the launchctl
// plumbing itself is exercised in internal/templates.
func respawnCheck(st templates.SupervisorState, looping bool) *DaemonRespawnCheck {
	c := NewDaemonRespawnCheck()
	c.probe = func(string) (templates.SupervisorState, bool) { return st, looping }
	return c
}

// writeRespawnBaseline writes the check's baseline file with a controlled
// spawn count, bypassing Run so a test can simulate a previous run.
func writeRespawnBaseline(t *testing.T, townRoot string, runs int) {
	t.Helper()
	path := daemonRespawnStatePath(townRoot)
	if err := writeDaemonRespawnBaseline(path, daemonRespawnBaseline{Runs: runs, RecordedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("writeDaemonRespawnBaseline: %v", err)
	}
}

// respawnLooping is the state gt-92qx3 was in: launchd knew the job, had no
// process for it, and every spawn died before the daemon logged anything. The
// canned 'launchctl print' output it is parsed from, and that its last exit
// code really is 78, are pinned in internal/templates'
// TestSupervisorRespawn.
var respawnLooping = templates.SupervisorState{
	Kind: "launchd", Loaded: true, PID: 0, Runs: 49, LastExit: templates.LaunchdExitConfig,
}

func TestDaemonRespawnCheck_LoopWithRisingRuns_Error(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRespawnBaseline(t, townRoot, 22) // the runs=22 a previous check saw

	result := respawnCheck(respawnLooping, true).Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	for _, want := range []string{"no pid", "last exit code 78", "runs 49", "rose from 22"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not name %q", result.Message, want)
		}
	}
	if !strings.Contains(result.FixHint, "launchctl bootout") || !strings.Contains(result.FixHint, "launchctl bootstrap") {
		t.Errorf("FixHint = %q, want the bootout and bootstrap repair", result.FixHint)
	}
}

// The first run on a looping host has no baseline to compare against: it still
// reports, because the state itself is the failure, and it records the count
// for the next run.
func TestDaemonRespawnCheck_LoopWithoutBaseline_StillError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	result := respawnCheck(respawnLooping, true).Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message: %s", result.Status, result.Message)
	}
	base, ok := readDaemonRespawnBaseline(daemonRespawnStatePath(townRoot))
	if !ok || base.Runs != 49 {
		t.Errorf("baseline = %+v (ok=%v), want runs 49 for the next run to compare against", base, ok)
	}
}

// A daemon that exited 78 once and has not been respawned yet is not a loop
// the check can name: RespawnLoop owns that judgement, and the probe here says
// it is not one.
func TestDaemonRespawnCheck_SpawnedOnce_NotALoop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	st := respawnLooping
	st.Runs = 1

	result := respawnCheck(st, false).Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message: %s", result.Status, result.Message)
	}
}

func TestDaemonRespawnCheck_DaemonRunning_OK(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	st := respawnLooping
	st.PID, st.Runs, st.LastExit = 64604, 53, 0

	result := respawnCheck(st, false).Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "64604") {
		t.Errorf("message %q does not name the pid", result.Message)
	}
}

// A probe that failed for a reason of its own proves nothing, so it is not a
// pass: an unreadable launchctl reading is exactly the state the incident left
// townhealth in.
func TestDaemonRespawnCheck_UnreadableState_Skipped(t *testing.T) {
	t.Parallel()
	st := templates.SupervisorState{Kind: "launchd", LastExit: -1, Err: errors.New("launchctl print: exec: not found")}

	result := respawnCheck(st, false).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped; message: %s", result.Status, result.Message)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "launchctl print") {
		t.Errorf("details %q do not carry the probe error", result.Details)
	}
}

// A host with no launchd job for the town has no respawn loop to find; the
// systemd kind is not this check's business either.
func TestDaemonRespawnCheck_NoLaunchdJob_OK(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"systemd", ""} {
		t.Run("kind="+kind, func(t *testing.T) {
			t.Parallel()
			result := respawnCheck(templates.SupervisorState{Kind: kind, LastExit: -1}, false).
				Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != StatusOK {
				t.Errorf("Status = %v, want StatusOK; message: %s", result.Status, result.Message)
			}
		})
	}
}
