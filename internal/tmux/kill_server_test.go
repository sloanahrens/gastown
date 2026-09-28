//go:build !windows

package tmux

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/constants"
)

// TestKillServerClearsLitterWithNoServer pins the ErrNoServer path: a kill that
// finds no server must still clear a file left where one belongs — which is
// exactly the state a run that died before reaching its cleanup leaves behind.
func TestKillServerClearsLitterWithNoServer(t *testing.T) {
	t.Parallel()
	socket := constants.TestSocketName("gt-test-killsrv-twice")
	socketPath := createStaleUnixSocket(t, socket)

	s := newScripted(bySub(map[string]reply{"kill-server": fail("no server running on " + socketPath)}))
	tm := newTmuxForTest(socket, s.exec, clockwork.NewFakeClock())
	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer with no server: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file survived a kill with no server: %v", err)
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
// server with no socket path to reach it — and a test may be holding that
// listener open on purpose (socket_guard_unix_test.go does exactly this).
func TestUnlinkDeadSocketFileKeepsLiveListener(t *testing.T) {
	t.Parallel()
	socket := constants.TestSocketName("gt-h9z-held")
	listener, socketPath := listenOnSocketPath(t, socket)
	defer func() { _ = listener.Close() }()

	clk := clockwork.NewFakeClock()
	done := make(chan struct{}, 1)
	go func() {
		unlinkDeadSocketFile(clk, socketPath)
		done <- struct{}{}
	}()
	driveClock(t, clk, socketUnlinkInterval, done)

	if _, err := os.Lstat(socketPath); err != nil {
		t.Errorf("unlinked a socket with a live listener: %v", err)
	}
}

// TestUnlinkDeadSocketFileRemovesStaleFile covers the case the fix exists for:
// the socket file is there, the server is not.
func TestUnlinkDeadSocketFileRemovesStaleFile(t *testing.T) {
	t.Parallel()
	socket := constants.TestSocketName("gt-h9z-gone")
	socketPath := createStaleUnixSocket(t, socket)

	unlinkDeadSocketFile(clockwork.NewFakeClock(), socketPath)

	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("stale socket file survived: %v", err)
	}
}

// TestUnlinkDeadSocketFileLeavesNonSocketFile pins the one state the unlink
// must not touch: a plain file sitting where a socket belongs. That is the
// signature gt-h9z guards against, and deciding it is litter is not this
// function's call.
func TestUnlinkDeadSocketFileLeavesNonSocketFile(t *testing.T) {
	t.Parallel()
	socket := constants.TestSocketName("gt-h9z-plainfile")
	socketPath := socketPathForTest(t, socket)
	if err := os.WriteFile(socketPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write %s: %v", socketPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	unlinkDeadSocketFile(clockwork.NewFakeClock(), socketPath)

	if _, err := os.Lstat(socketPath); err != nil {
		t.Errorf("unlinked a file that was never a socket: %v", err)
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
	tm := newTmuxForTest("", s.exec, clockwork.NewFakeClock())
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
	socket := constants.TestSocketName("gt-h9z-never-bound")
	socketPath := socketPathForTest(t, socket)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("pre-clean %s: %v", socketPath, err)
	}

	done := make(chan struct{})
	go func() {
		unlinkDeadSocketFile(clockwork.NewFakeClock(), socketPath)
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("unlink of a missing path blocked")
	}
}
