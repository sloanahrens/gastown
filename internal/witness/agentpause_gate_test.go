package witness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/testutil"
)

// stubRestartSessionExec replaces the session-restart seam so a test can see
// exactly what RestartPolecatSession decided, and returns the addresses it was
// asked to restart.
//
// The seam exists so these tests never spawn a subprocess: a real
// `gt session restart <addr> --force` from a non-hermetic test process can
// resolve to the live town and restart a real session (gt-wisp-6ajo).
//
// Must not be combined with t.Parallel: it mutates a package variable.
func stubRestartSessionExec(t *testing.T) *[]string {
	t.Helper()
	restarts := &[]string{}
	old := restartSessionExec
	restartSessionExec = func(_, address string) error {
		*restarts = append(*restarts, address)
		return nil
	}
	t.Cleanup(func() { restartSessionExec = old })
	return restarts
}

// TestRestartPolecatSessionHonoursPause (gt-ahik): an operator-sanctioned
// pause (marker file from `gt agent pause`) must make RestartPolecatSession
// a no-op, not a spawn — this is the choke point that closed the
// done-intent-dead / zombie / staleness respawn instances.
func TestRestartPolecatSessionHonoursPause(t *testing.T) {
	town := testutil.HermeticTest(t)
	restarts := stubRestartSessionExec(t)

	if err := agentpause.Pause(town, "gastown", "polecat", "flint", "frozen by operator", "human", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if err := RestartPolecatSession(town, "gastown", "flint"); err != nil {
		t.Fatalf("RestartPolecatSession on paused polecat returned error (want silent no-op): %v", err)
	}

	if len(*restarts) != 0 {
		t.Errorf("restart executed for a paused agent: %v (want no restart at all)", *restarts)
	}
	paused, _, perr := agentpause.IsPaused(town, "gastown", "polecat", "flint")
	if perr != nil {
		t.Fatalf("IsPaused: %v", perr)
	}
	if !paused {
		t.Fatal("pause marker was cleared by the restart path — pause not honored")
	}
}

// TestRestartPolecatSessionRestartsUnpausedAgent is the control for the test
// above: with no pause recorded, the gate must let the restart through. It
// asserts the restart itself, so a gate that short-circuited everything —
// which would satisfy the pause test on its own — fails here.
func TestRestartPolecatSessionRestartsUnpausedAgent(t *testing.T) {
	town := testutil.HermeticTest(t)
	restarts := stubRestartSessionExec(t)

	if err := RestartPolecatSession(town, "gastown", "flint"); err != nil {
		t.Fatalf("RestartPolecatSession on unpaused polecat: %v", err)
	}

	if len(*restarts) != 1 {
		t.Fatalf("restart calls = %v, want exactly one", *restarts)
	}
	if (*restarts)[0] != "gastown/flint" {
		t.Errorf("restarted %q, want %q", (*restarts)[0], "gastown/flint")
	}
}

// TestRestartPolecatSessionFailsClosed pins the gate's failure mode
// (gt-wisp-6ajo): when the pause state cannot be read, the restart must not
// happen. A marker that is truncated (crash mid-write, hand-edit) or
// unreadable (a directory, a permission problem) reads as paused, because
// restarting the agent an operator parked is the harm this gate exists to
// prevent.
func TestRestartPolecatSessionFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, markerPath string)
	}{
		{
			name: "truncated marker",
			damage: func(t *testing.T, markerPath string) {
				writeMarkerFile(t, markerPath, `{"paused": tr`)
			},
		},
		{
			name: "empty marker",
			damage: func(t *testing.T, markerPath string) {
				writeMarkerFile(t, markerPath, "")
			},
		},
		{
			name: "unreadable marker",
			damage: func(t *testing.T, markerPath string) {
				if err := os.MkdirAll(markerPath, 0o755); err != nil {
					t.Fatalf("creating marker directory: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			town := testutil.HermeticTest(t)
			restarts := stubRestartSessionExec(t)

			markerPath := agentpause.FilePath(town, "gastown", "polecat", "flint")
			if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
				t.Fatalf("creating marker dir: %v", err)
			}
			tc.damage(t, markerPath)

			if err := RestartPolecatSession(town, "gastown", "flint"); err != nil {
				t.Fatalf("RestartPolecatSession returned error (want silent skip): %v", err)
			}
			if len(*restarts) != 0 {
				t.Errorf("restart executed despite unreadable pause state: %v (must fail closed)", *restarts)
			}
		})
	}
}

func writeMarkerFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing marker %s: %v", path, err)
	}
}

// TestPauseGateSkip pins the choke point DetectZombiePolecats calls before
// any zombie classification or pre-restart side effect (done-intent label
// clearing, cleanup wisp creation, the aa-apw archive/nuke path) runs for a
// polecat (gt-ahik, om review on gt-wisp-2pes: those side effects ran even
// when the gate inside RestartPolecatSession later skipped the restart
// itself). pauseGateSkip must report true — skip this polecat entirely —
// whenever PauseGate would refuse to touch the agent, paused or unreadable
// alike (fail closed), and false only when the agent is verifiably unpaused.
func TestPauseGateSkip(t *testing.T) {
	town := testutil.HermeticTest(t)

	if pauseGateSkip(town, "gastown", "flint") {
		t.Fatal("no marker: pauseGateSkip = true, want false (nothing to skip for)")
	}

	if err := agentpause.Pause(town, "gastown", "polecat", "flint", "frozen by operator", "human", "working"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !pauseGateSkip(town, "gastown", "flint") {
		t.Fatal("paused: pauseGateSkip = false, want true")
	}

	if err := agentpause.Resume(town, "gastown", "polecat", "flint"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if pauseGateSkip(town, "gastown", "flint") {
		t.Fatal("resumed: pauseGateSkip = true, want false")
	}

	markerPath := agentpause.FilePath(town, "gastown", "polecat", "flint")
	writeMarkerFile(t, markerPath, `{"paused": tr`)
	if !pauseGateSkip(town, "gastown", "flint") {
		t.Fatal("malformed marker: pauseGateSkip = false, want true (fail closed)")
	}
}
