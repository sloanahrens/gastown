package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
)

// TestDeliberateStopMarkerCoordinatesAreTheDetectorGate pins the one thing
// gt session stop and the witness zombie detector have to agree on (gt-fojqs):
// the marker file. The detector's pauseGateSkip reads
// agentpause.PauseGate(townRoot, rigName, polecat, polecatName); the writer
// must land on exactly that path, or the operator's stop records nothing and
// the detector restarts the session anyway.
func TestDeliberateStopMarkerCoordinatesAreTheDetectorGate(t *testing.T) {
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	wrote, err := writeDeliberateStopMarker(townRoot, rigName, polecatName)
	if err != nil {
		t.Fatalf("writeDeliberateStopMarker: %v", err)
	}
	if !wrote {
		t.Fatal("writeDeliberateStopMarker wrote nothing on a clean polecat")
	}

	// The witness gate's own coordinates, spelled out: this is the path the
	// detector reads, not a path this package chose.
	want := filepath.Join(townRoot, ".runtime", "agents", rigName, "polecat."+polecatName+".json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("marker not at the path the detector's gate reads (%s): %v", want, err)
	}

	paused, st, perr := agentpause.PauseGate(townRoot, rigName, "polecat", polecatName)
	if perr != nil {
		t.Fatalf("PauseGate: %v", perr)
	}
	if !paused {
		t.Fatal("PauseGate = false after a deliberate stop; the detector would restart the polecat")
	}
	if st == nil || st.Reason != deliberateSessionStopReason {
		t.Fatalf("marker reason = %v, want %q", st, deliberateSessionStopReason)
	}

	if !polecatSessionParked(townRoot, rigName, polecatName) {
		t.Error("polecatSessionParked = false for a polecat the operator just stopped")
	}
}

// TestDeliberateStopMarkerKeepsAnExistingPause: a polecat parked by gt agent
// pause is already covered by the gate, and its reason is the operator's.
// Rewriting it with the stop reason would lose that (gt-fojqs).
func TestDeliberateStopMarkerKeepsAnExistingPause(t *testing.T) {
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	if err := agentpause.Pause(townRoot, rigName, "polecat", polecatName, "operator hold", "human", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	wrote, err := writeDeliberateStopMarker(townRoot, rigName, polecatName)
	if err != nil {
		t.Fatalf("writeDeliberateStopMarker: %v", err)
	}
	if wrote {
		t.Error("writeDeliberateStopMarker rewrote an existing pause marker")
	}

	_, st, _ := agentpause.PauseGate(townRoot, rigName, "polecat", polecatName)
	if st == nil || st.Reason != "operator hold" {
		t.Errorf("reason = %v, want the pause's own reason %q", st, "operator hold")
	}
}

// TestDeliberateStopMarkerWithoutTownRoot: the marker path is built from the
// town root, so a missing one must fail rather than write into the caller's
// directory, where no scanner looks.
func TestDeliberateStopMarkerWithoutTownRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	wrote, werr := writeDeliberateStopMarker("", "gastown", "garnet")
	if werr == nil {
		t.Error("writeDeliberateStopMarker with no town root returned no error")
	}
	if wrote {
		t.Error("writeDeliberateStopMarker reported a write with no town root")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".runtime")); err == nil {
		t.Errorf("wrote %s/.runtime with no town root", cwd)
	}
}

// TestClearParkedSession: an explicit start (gt session start or gt
// session restart) puts the polecat back under the detector's watch, and
// clearing is idempotent — a start on a polecat nobody stopped clears nothing
// and reports nothing (gt-fojqs).
func TestClearParkedSession(t *testing.T) {
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	if _, err := writeDeliberateStopMarker(townRoot, rigName, polecatName); err != nil {
		t.Fatalf("writeDeliberateStopMarker: %v", err)
	}

	if !clearParkedSession(townRoot, rigName, polecatName) {
		t.Fatal("clearParkedSession = false with a marker present")
	}
	if polecatSessionParked(townRoot, rigName, polecatName) {
		t.Fatal("polecat still parked after the marker was cleared; the detector would never recover it")
	}
	if clearParkedSession(townRoot, rigName, polecatName) {
		t.Error("clearParkedSession = true with no marker present")
	}
	if clearParkedSession("", rigName, polecatName) {
		t.Error("clearParkedSession with no town root reported a clear")
	}
}

// TestStartPolecatsWithWorkSkipsParkedPolecat: `gt up --restore` starts
// polecats that have pinned work, so a town restart is another way a
// deliberately stopped polecat comes back (gt-fojqs). The fake tmux fails
// session creation, so an unskipped polecat lands in the error map — that is
// the control proving this test can see a start being attempted.
func TestStartPolecatsWithWorkSkipsParkedPolecat(t *testing.T) {
	townRoot := setupTestTownForDotDir(t)
	rigName := "gastown"
	rigPath := filepath.Join(townRoot, rigName)

	addRigEntry(t, townRoot, rigName)

	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", "garnet"), 0755); err != nil {
		t.Fatalf("mkdir polecat: %v", err)
	}

	binDir := t.TempDir()
	writeScript(t, binDir, "bd", `#!/bin/sh
cmd="$1"
case "$cmd" in
  list)
    echo '[{"id":"gt-1","status":"pinned"}]'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`)
	writeScript(t, binDir, "tmux", `#!/bin/sh
case "$1" in
  has-session)
    echo "can't find session" 1>&2
    exit 1
    ;;
  *)
    echo "tmux: refused by test" 1>&2
    exit 1
    ;;
esac
`)
	t.Setenv("PATH", fmt.Sprintf("%s:%s", binDir, os.Getenv("PATH")))

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir town root: %v", err)
	}

	// Control: unparked, the polecat is started and the failure reaches the
	// caller (proving the parked run below is a skip, not a dead harness).
	_, controlErrs := startPolecatsWithWork(townRoot, rigName)
	if len(controlErrs) == 0 {
		t.Fatal("unparked control: no error from a failing tmux; the harness cannot see a start attempt")
	}

	if _, err := writeDeliberateStopMarker(townRoot, rigName, "garnet"); err != nil {
		t.Fatalf("writeDeliberateStopMarker: %v", err)
	}

	started, errs := startPolecatsWithWork(townRoot, rigName)

	if len(started) != 0 {
		t.Errorf("started = %v, want none: the polecat is parked", started)
	}
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none: a parked polecat must not be started at all", errs)
	}
	if !polecatSessionParked(townRoot, rigName, "garnet") {
		t.Error("the marker was cleared by the start path; gt session start is the place that clears it")
	}
}
