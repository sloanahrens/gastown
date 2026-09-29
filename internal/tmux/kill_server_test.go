//go:build !windows

package tmux

import (
	"testing"
)

// TestKillServerClearsLitterWithNoServer pins the ErrNoServer path: a kill that
// finds no server must still clear a file left where one belongs — which is
// exactly the state a run that died before reaching its cleanup leaves behind.
func TestKillServerClearsLitterWithNoServer(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/fake-sock/gt-test-killsrv", sockStale)
	s := newScripted(bySub(map[string]reply{"kill-server": fail("no server running on /fake-sock/gt-test-killsrv")}))
	tm := socketTmux("gt-test-killsrv", fs, s)
	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer with no server: %v", err)
	}
	if fs.state("/fake-sock/gt-test-killsrv") != sockAbsent {
		t.Error("socket file survived a kill with no server")
	}
}

// TestKillServerReportsOtherErrors keeps KillServer from swallowing a failure
// that is not "no server".
func TestKillServerReportsOtherErrors(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"kill-server": fail("permission denied")}))
	if err := unitTmux(s, nil).KillServer(); err == nil {
		t.Fatal("KillServer = nil, want the tmux error")
	}
}

// TestUnlinkDeadSocketFileKeepsLiveListener is the guard on the unlink: a file
// with something still listening is not residue. Removing it would strand a
// server with no socket path to reach it. It re-checks for socketUnlinkWait
// (a just-killed server answers a little longer) and then leaves the file.
func TestUnlinkDeadSocketFileKeepsLiveListener(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/s/held", sockLive)
	clk := newFixedClock()
	done := make(chan struct{}, 1)
	go func() {
		unlinkDeadSocketFile(clk, *fs.ops(), "/s/held")
		done <- struct{}{}
	}()
	driveClock(t, clk, socketUnlinkInterval, done)
	if fs.state("/s/held") != sockLive {
		t.Error("unlinked a socket with a live listener")
	}
}

// TestUnlinkDeadSocketFileWaitsOutAClosingServer: a server that stops
// answering within socketUnlinkWait has its file removed.
func TestUnlinkDeadSocketFileWaitsOutAClosingServer(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/s/closing", sockStale)
	fs.queueDial("/s/closing", nil, nil, nil) // still accepting for three checks
	clk := newFixedClock()
	done := make(chan struct{}, 1)
	go func() {
		unlinkDeadSocketFile(clk, *fs.ops(), "/s/closing")
		done <- struct{}{}
	}()
	driveClock(t, clk, socketUnlinkInterval, done)
	if fs.state("/s/closing") != sockAbsent {
		t.Error("socket file of a server that stopped answering survived")
	}
}

// TestUnlinkDeadSocketFileRemovesStaleFile covers the case the fix exists for:
// the socket file is there, the server is not.
func TestUnlinkDeadSocketFileRemovesStaleFile(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/s/gone", sockStale)
	unlinkDeadSocketFile(newFixedClock(), *fs.ops(), "/s/gone")
	if fs.state("/s/gone") != sockAbsent {
		t.Error("stale socket file survived")
	}
}

// TestUnlinkDeadSocketFileLeavesNonSocketFile pins the one state the unlink
// must not touch: a plain file sitting where a socket belongs. That is the
// signature gt-h9z guards against, and deciding it is litter is not this
// function's call.
func TestUnlinkDeadSocketFileLeavesNonSocketFile(t *testing.T) {
	t.Parallel()
	for _, kernel := range []struct {
		name  string
		linux bool
	}{{"darwin", false}, {"linux", true}} {
		for _, st := range []sockState{sockFile, sockDir, sockSymlink} {
			fs := newFakeSockets()
			// On Linux the dial of a non-socket is refused exactly like a dead
			// socket's, so the refusal alone cannot tell them apart; the
			// nightly deleted a plain file this way (gt-22hdp.39).
			fs.linuxConnect = kernel.linux
			fs.set("/s/plain", st)
			unlinkDeadSocketFile(newFixedClock(), *fs.ops(), "/s/plain")
			if fs.state("/s/plain") != st {
				t.Errorf("%s: unlinked a path (state %d) that was never a socket", kernel.name, st)
			}
		}
	}
}

// TestOwnsSocketFile is the line the unlink must not cross: the default and
// sentinel socket paths belong to servers this wrapper never started, and the
// default path is the town's own live server.
func TestOwnsSocketFile(t *testing.T) {
	t.Parallel()
	if (&Tmux{}).ownsSocketFile() {
		t.Error("a wrapper with no socket name must not claim the default socket's file")
	}
	if NewTmuxWithSocket("default").ownsSocketFile() {
		t.Error("the default socket's file belongs to whatever server bound it")
	}
	if NewTmuxWithSocket(noTownSocket).ownsSocketFile() {
		t.Error("the no-town sentinel names no real socket")
	}
	if !NewTmuxWithSocket("gt-test-1234").ownsSocketFile() {
		t.Error("a named socket is the caller's own server")
	}
}

// TestKillServerLeavesTownSocketAlone is the safety property the guard exists
// for: a wrapper with no socket name sends kill-server and stops there, so
// `gt down` and anything else killing through a default-socket wrapper cannot
// delete the town's socket path.
func TestKillServerLeavesTownSocketAlone(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"kill-server": fail("no server running")}))
	tm := newTmuxForTest("", s.exec, newFixedClock())
	if tm.ownsSocketFile() {
		t.Fatal("default-socket wrapper would unlink the town socket file")
	}
	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	if got := s.all(); len(got) != 1 || !got[0].has("kill-server") || got[0].has("-L") {
		t.Fatalf("calls = %v, want one socketless kill-server", got)
	}
}

// TestUnlinkDeadSocketFileMissingPathIsQuiet pins the "already gone" path: a
// server that unlinked its own socket between the kill and the sweep is not an
// error, and it returns without waiting on the clock.
func TestUnlinkDeadSocketFileMissingPathIsQuiet(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	err := returnsWithoutClock(t, func() error {
		unlinkDeadSocketFile(newFixedClock(), *fs.ops(), "/s/never-bound")
		return nil
	})
	if err != nil || len(fs.removed) != 0 {
		t.Fatalf("err = %v, removed = %v", err, fs.removed)
	}
}
