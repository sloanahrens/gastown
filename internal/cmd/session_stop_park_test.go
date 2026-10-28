package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// TestStopParksSeat is the one rule gt session stop applies to decide whether
// it parks: a sandbox seat parks only when --park asks (gt-itmpq), a host seat
// parks as it always has, and a registry read that failed parks too, because
// restarting a seat the operator stopped is the incident the park prevents.
func TestStopParksSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		sandboxed   bool
		readFailed  bool
		park        bool
		wantParking bool
	}{
		{name: "host seat parks on a plain stop", wantParking: true},
		{name: "host seat parks with --park", park: true, wantParking: true},
		{name: "sandbox seat does not park on a plain stop", sandboxed: true, wantParking: false},
		{name: "sandbox seat parks with --park", sandboxed: true, park: true, wantParking: true},
		{name: "unreadable registry parks", readFailed: true, wantParking: true},
		{name: "unreadable registry of a sandbox seat parks", sandboxed: true, readFailed: true, wantParking: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stopParksSeat(tt.sandboxed, tt.readFailed, tt.park); got != tt.wantParking {
				t.Fatalf("stopParksSeat(sandboxed=%v, readFailed=%v, park=%v) = %v, want %v",
					tt.sandboxed, tt.readFailed, tt.park, got, tt.wantParking)
			}
		})
	}
}

// recordSandboxSeat records sessionID as a sandbox seat in townRoot's
// registry, the record removeSandbox and IsSandboxed share.
func recordSandboxSeat(t *testing.T, townRoot, rigName, polecatName, sessionID string) {
	t.Helper()
	if err := session.WriteSandboxRecord(townRoot, session.SandboxRecord{
		Session: sessionID,
		Rig:     rigName,
		Polecat: polecatName,
		Profile: "deepseek",
	}); err != nil {
		t.Fatalf("WriteSandboxRecord: %v", err)
	}
}

// TestSandboxSeatStopLeavesNoParkMarker: a plain stop of a sandbox seat records
// nothing that parks it, so the reuse gate the next sling runs accepts the seat
// (gt-itmpq). The host-seat control in the same test proves the gate can see a
// park at all.
func TestSandboxSeatStopLeavesNoParkMarker(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"
	sessionID := "gt-gastown-garnet"

	recordSandboxSeat(t, townRoot, rigName, polecatName, sessionID)

	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false)
	if rec.Written || rec.Parked {
		t.Fatalf("recordDeliberateStop parked a sandbox seat: %+v", rec)
	}
	if !rec.Sandbox {
		t.Fatal("recordDeliberateStop did not read the seat as a sandbox seat")
	}
	if parked, _, _ := agentpause.PauseGate(townRoot, rigName, "polecat", polecatName); parked {
		t.Fatal("a park marker is on the seat after a plain stop of a sandbox seat")
	}
	if reason, parked := polecat.ParkedReuseBlocker(townRoot, rigName, polecatName); parked {
		t.Fatalf("the reuse gate refuses the stopped sandbox seat (%s); the next sling would not be accepted", reason)
	}

	// Control: the same gate refuses a parked seat, so the check above is a
	// reading, not a gate that can never answer.
	if _, err := writeDeliberateStopMarker(townRoot, rigName, "onyx"); err != nil {
		t.Fatalf("writeDeliberateStopMarker: %v", err)
	}
	if _, parked := polecat.ParkedReuseBlocker(townRoot, rigName, "onyx"); !parked {
		t.Fatal("the reuse gate accepted a parked polecat; the check above proves nothing")
	}
}

// TestSandboxSeatStopParksWithPark: --park on a sandbox seat records the same
// deliberate stop a host seat gets, so patrol_scan and the zombie detector
// leave it down.
func TestSandboxSeatStopParksWithPark(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"
	sessionID := "gt-gastown-garnet"

	recordSandboxSeat(t, townRoot, rigName, polecatName, sessionID)

	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, true)
	if !rec.Written || !rec.Parked || !rec.Sandbox {
		t.Fatalf("recordDeliberateStop with --park on a sandbox seat = %+v, want written, parked and sandbox", rec)
	}
	paused, st, err := agentpause.PauseGate(townRoot, rigName, "polecat", polecatName)
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}
	if !paused {
		t.Fatal("--park wrote no park marker on a sandbox seat")
	}
	if st == nil || st.Reason != deliberateSessionStopReason {
		t.Fatalf("marker reason = %v, want %q", st, deliberateSessionStopReason)
	}
}

// TestHostSeatStopStillParks: a seat with no sandbox record is a host tmux
// seat, and its stop parks exactly as before this change.
func TestHostSeatStopStillParks(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"
	sessionID := "gt-gastown-garnet"

	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false)
	if !rec.Written || !rec.Parked || rec.Sandbox {
		t.Fatalf("recordDeliberateStop on a host seat = %+v, want written and parked, not a sandbox seat", rec)
	}
	if !polecatSessionParked(townRoot, rigName, polecatName) {
		t.Fatal("a host seat's stop left no park marker")
	}
}

// TestUnreadableRegistryParksTheSeat: a registry that cannot be read is not a
// host seat. The stop parks and warns, rather than reporting a sandbox seat it
// could not prove, and it still stops.
func TestUnreadableRegistryParksTheSeat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"
	sessionID := "gt-gastown-garnet"

	dir := session.SandboxRegistryDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write broken record: %v", err)
	}

	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false)
	if !rec.Parked || !rec.Written {
		t.Fatalf("recordDeliberateStop with an unreadable registry = %+v, want the seat parked", rec)
	}
	if rec.Sandbox {
		t.Fatal("an unreadable registry was reported as a sandbox seat")
	}
	if !polecatSessionParked(townRoot, rigName, polecatName) {
		t.Fatal("an unreadable registry left the seat unmarked; the patrol would restart a stopped polecat")
	}
}
