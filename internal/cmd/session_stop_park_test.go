package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
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

// seatSessionName derives a seat's session name through the naming gt session
// stop itself asks for (polecat.SessionManager.SessionName), so a test writes
// the sandbox registry key production looks a seat up by, not a shape the test
// invented.
func seatSessionName(t *testing.T, rigName, polecatName string) string {
	t.Helper()
	mgr := polecat.NewSessionManager(nil, &rig.Rig{Name: rigName}, cmdTestRegistry())
	return mgr.SessionName(polecatName)
}

// recordSandboxSeat records a sandbox seat in townRoot's registry, the record
// removeSandbox deletes with the sandbox, and returns the session name it was
// recorded under.
func recordSandboxSeat(t *testing.T, townRoot, rigName, polecatName string) string {
	t.Helper()
	sessionID := seatSessionName(t, rigName, polecatName)
	if err := session.WriteSandboxRecord(townRoot, session.SandboxRecord{
		Session: sessionID,
		Rig:     rigName,
		Polecat: polecatName,
		Profile: "deepseek",
	}); err != nil {
		t.Fatalf("WriteSandboxRecord: %v", err)
	}
	return sessionID
}

// TestSandboxSeatStopLeavesNoParkMarker: a plain stop of a sandbox seat records
// nothing that parks it, so the reuse gate the next sling runs accepts the seat
// (gt-itmpq). The host-seat control in the same test proves the gate can see a
// park at all.
func TestSandboxSeatStopLeavesNoParkMarker(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	sessionID := recordSandboxSeat(t, townRoot, rigName, polecatName)

	var warn strings.Builder
	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false, &warn)
	if rec.Written || rec.Parked {
		t.Fatalf("recordDeliberateStop parked a sandbox seat: %+v", rec)
	}
	if !rec.Sandbox {
		t.Fatal("recordDeliberateStop did not read the seat as a sandbox seat")
	}
	if warn.Len() != 0 {
		t.Fatalf("a plain stop of a sandbox seat warned: %q", warn.String())
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

// TestSandboxSeatStopReportsAnEarlierPark: a sandbox seat an earlier gt agent
// pause parked still carries that marker after a plain stop, and the stop says
// so rather than telling the operator the next sling can reuse the seat the
// reuse gate refuses.
func TestSandboxSeatStopReportsAnEarlierPark(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	sessionID := recordSandboxSeat(t, townRoot, rigName, polecatName)
	if err := agentpause.Pause(townRoot, rigName, "polecat", polecatName, "operator park", "test", ""); err != nil {
		t.Fatalf("agentpause.Pause: %v", err)
	}

	var warn strings.Builder
	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false, &warn)
	if rec.Written {
		t.Fatalf("a plain stop of a parked sandbox seat wrote its own marker: %+v", rec)
	}
	if !rec.Parked || !rec.Sandbox {
		t.Fatalf("recordDeliberateStop on a parked sandbox seat = %+v, want it reported as parked", rec)
	}
	if _, parked := polecat.ParkedReuseBlocker(townRoot, rigName, polecatName); !parked {
		t.Fatal("the reuse gate accepts a seat an earlier park holds; the reported park is wrong")
	}
}

// TestSandboxSeatStopParksWithPark: --park on a sandbox seat records the same
// deliberate stop a host seat gets, so patrol_scan and the zombie detector
// leave it down.
func TestSandboxSeatStopParksWithPark(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	sessionID := recordSandboxSeat(t, townRoot, rigName, polecatName)

	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, true, &strings.Builder{})
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

	sessionID := seatSessionName(t, rigName, polecatName)

	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false, &strings.Builder{})
	if !rec.Written || !rec.Parked || rec.Sandbox {
		t.Fatalf("recordDeliberateStop on a host seat = %+v, want written and parked, not a sandbox seat", rec)
	}
	if !polecatSessionParked(townRoot, rigName, polecatName) {
		t.Fatal("a host seat's stop left no park marker")
	}
}

// TestUnreadableRegistryParksTheSeat: a registry that cannot be read is not a
// host seat. The stop parks and says so, rather than reporting a sandbox seat
// it could not prove, and it still stops.
func TestUnreadableRegistryParksTheSeat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	sessionID := seatSessionName(t, rigName, polecatName)

	dir := session.SandboxRegistryDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write broken record: %v", err)
	}

	var warn strings.Builder
	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false, &warn)
	if !rec.Parked || !rec.Written {
		t.Fatalf("recordDeliberateStop with an unreadable registry = %+v, want the seat parked", rec)
	}
	if rec.Sandbox {
		t.Fatal("an unreadable registry was reported as a sandbox seat")
	}

	// The alarm is the only thing that tells the operator why a seat they may
	// have believed was a sandbox seat is parked, so it must reach the warning
	// writer the command points at stderr.
	for _, want := range []string{
		"could not tell whether " + sessionID + " runs in a sandbox",
		"recording the stop as a park",
	} {
		if !strings.Contains(warn.String(), want) {
			t.Fatalf("warning from an unreadable registry does not say %q: %q", want, warn.String())
		}
	}

	if !polecatSessionParked(townRoot, rigName, polecatName) {
		t.Fatal("an unreadable registry left the seat unmarked; the patrol would restart a stopped polecat")
	}
}

// fixedStopSessions is the session manager gt session stop drives in these
// tests: the seat exists, it answers to the recorded session, and its Stop
// fails with stopErr so the stop's own handling of a failed stop runs.
func fixedStopSessions(sessionID string, stopErr error) func(*rig.Rig) stopSeatManager {
	return func(*rig.Rig) stopSeatManager {
		return fixedStopSeat{sessionID: sessionID, stopErr: stopErr}
	}
}

type fixedStopSeat struct {
	sessionID string
	stopErr   error
}

func (s fixedStopSeat) HasPolecat(string) bool    { return true }
func (s fixedStopSeat) SessionName(string) string { return s.sessionID }
func (s fixedStopSeat) Stop(string, bool) error   { return s.stopErr }

// TestSandboxStopErrorKeepsThePark drives gt session stop itself: a stop that
// ended the session but left its sandbox behind returns
// polecat.ErrSandboxNotRemoved, and the marker this run wrote stays, because
// the seat really is stopped and the patrol must leave it down (gt-nbo3y). A
// stop error that stopped nothing still withdraws the marker, or a polecat
// that was never stopped would read as deliberately stopped.
//
//testpolicy:allow parallel — runSessionStop reads the --park flag variable, the working directory and its session manager seam, all process-wide
func TestSandboxStopErrorKeepsThePark(t *testing.T) {
	tests := []struct {
		name string
		// park is --park: the stop writes its own marker before it runs.
		park bool
		// prePaused is an earlier gt agent pause, a marker this run did not write.
		prePaused  bool
		stopErr    error
		wantParked bool
	}{
		{
			name:       "sandbox left behind keeps the park",
			park:       true,
			stopErr:    fmt.Errorf("removing its sandbox: %w", polecat.ErrSandboxNotRemoved),
			wantParked: true,
		},
		{
			name:       "stop that stopped nothing withdraws the park",
			park:       true,
			stopErr:    errors.New("killing session: no such session"),
			wantParked: false,
		},
		{
			name:       "an earlier park survives a failed stop this run did not park",
			prePaused:  true,
			stopErr:    errors.New("killing session: no such session"),
			wantParked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rigName, polecatName := "gastown", "garnet"
			townRoot := setupSessionSeatTown(t, polecatName)

			// The seat as gt session stop sees it: a sandbox seat whose marker,
			// if any, was written before the stop ran.
			sessionID := recordSandboxSeat(t, townRoot, rigName, polecatName)
			if tt.prePaused {
				if err := agentpause.Pause(townRoot, rigName, "polecat", polecatName, "operator park", "test", ""); err != nil {
					t.Fatalf("agentpause.Pause: %v", err)
				}
			}

			//testpolicy:allow no-chdir — the town gt session stop resolves is the working directory it runs in
			t.Chdir(townRoot)
			park, sessions := sessionPark, newStopSessions
			t.Cleanup(func() {
				//testpolicy:allow no-global-swap — restores the --park flag and the session manager swapped below
				sessionPark, newStopSessions = park, sessions
			})
			//testpolicy:allow no-global-swap — runSessionStop reads the --park flag variable; it is not an argument
			sessionPark = tt.park
			//testpolicy:allow no-global-swap — runSessionStop builds its session manager from this seam; it is not an argument
			newStopSessions = fixedStopSessions(sessionID, tt.stopErr)

			err := runSessionStop(nil, []string{rigName + "/" + polecatName})
			if err == nil {
				t.Fatal("gt session stop over a failed stop = nil, want the stop's error")
			}
			if leftBehind := errors.Is(err, polecat.ErrSandboxNotRemoved); leftBehind != errors.Is(tt.stopErr, polecat.ErrSandboxNotRemoved) {
				t.Fatalf("gt session stop error = %v, want it to carry ErrSandboxNotRemoved = %v",
					err, errors.Is(tt.stopErr, polecat.ErrSandboxNotRemoved))
			}

			if parked := polecatSessionParked(townRoot, rigName, polecatName); parked != tt.wantParked {
				t.Fatalf("parked after the failed stop = %v, want %v", parked, tt.wantParked)
			}
		})
	}
}

// TestMarkerWriteFailureLeavesTheSeatUnparked: when the park marker cannot be
// written the seat is not parked, and the stop says the restart may bring it
// back instead of reporting a park the patrol cannot see.
func TestMarkerWriteFailureLeavesTheSeatUnparked(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName, polecatName := "gastown", "garnet"

	sessionID := seatSessionName(t, rigName, polecatName)

	// A file where the marker's lock directory belongs fails the write while
	// leaving the marker absent, which is the branch a full disk or a bad
	// town root would take.
	locks := filepath.Join(townRoot, ".runtime", "locks", "intent")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir lock parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, rigName), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("block the lock dir: %v", err)
	}

	var warn strings.Builder
	rec := recordDeliberateStop(townRoot, rigName, polecatName, sessionID, false, &warn)
	if rec.Written || rec.Parked {
		t.Fatalf("a failed marker write reported a park: %+v", rec)
	}
	if !strings.Contains(warn.String(), "could not record the stop for "+rigName+"/"+polecatName) {
		t.Fatalf("a failed marker write did not warn that the restart may bring the seat back: %q", warn.String())
	}
	if polecatSessionParked(townRoot, rigName, polecatName) {
		t.Fatal("the seat reads as parked with no marker written")
	}
}
