//go:build !windows

package tmux

import (
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// TestKillServerUnlinksSocketFile is the gt-20di fix at its source: tmux leaves
// the socket file behind when a server exits, so a test package that kills its
// server at the end of every run added one file forever. KillServer now clears
// the file too.
func TestKillServerUnlinksSocketFile(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := constants.TestSocketName("gt-test-killsrv")
	socketPath := socketPathForTest(t, socket)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("pre-clean %s: %v", socketPath, err)
	}

	tm := NewTmuxWithSocket(socket)
	if err := tm.NewSessionWithCommand("gt-test-killsrv-1", ".", "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("socket file missing while the server is up: %v", err)
	}

	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file survived KillServer: %v", err)
	}
}

// TestKillServerClearsLitterWithNoServer pins the ErrNoServer path: a kill that
// finds no server must still clear a file left where one belongs — which is
// exactly the state a run that died before reaching its cleanup leaves behind.
func TestKillServerClearsLitterWithNoServer(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := constants.TestSocketName("gt-test-killsrv-twice")
	socketPath := createStaleUnixSocket(t, socket)

	tm := NewTmuxWithSocket(socket)
	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer with no server: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file survived a kill with no server: %v", err)
	}
}

// TestUnlinkDeadSocketFileKeepsLiveListener is the guard on the unlink: a file
// with something still listening is not residue. Removing it would strand a
// server with no socket path to reach it — and a test may be holding that
// listener open on purpose (socket_guard_unix_test.go does exactly this).
func TestUnlinkDeadSocketFileKeepsLiveListener(t *testing.T) {
	socket := constants.TestSocketName("gt-h9z-held")
	listener, socketPath := listenOnSocketPath(t, socket)
	defer func() { _ = listener.Close() }()

	unlinkDeadSocketFile(socketPath)

	if _, err := os.Lstat(socketPath); err != nil {
		t.Errorf("unlinked a socket with a live listener: %v", err)
	}
}

// TestUnlinkDeadSocketFileRemovesStaleFile covers the case the fix exists for:
// the socket file is there, the server is not.
func TestUnlinkDeadSocketFileRemovesStaleFile(t *testing.T) {
	socket := constants.TestSocketName("gt-h9z-gone")
	socketPath := createStaleUnixSocket(t, socket)

	unlinkDeadSocketFile(socketPath)

	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("stale socket file survived: %v", err)
	}
}

// TestUnlinkDeadSocketFileLeavesNonSocketFile pins the one state the unlink
// must not touch: a plain file sitting where a socket belongs. That is the
// signature gt-h9z guards against, and deciding it is litter is not this
// function's call.
func TestUnlinkDeadSocketFileLeavesNonSocketFile(t *testing.T) {
	socket := constants.TestSocketName("gt-h9z-plainfile")
	socketPath := socketPathForTest(t, socket)
	if err := os.WriteFile(socketPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write %s: %v", socketPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	unlinkDeadSocketFile(socketPath)

	if _, err := os.Lstat(socketPath); err != nil {
		t.Errorf("unlinked a file that was never a socket: %v", err)
	}
}

// TestOwnsSocketFile is the line the unlink must not cross: the default and
// sentinel socket paths belong to servers this wrapper never started, and the
// default path is the town's own live server.
func TestOwnsSocketFile(t *testing.T) {
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
// for, checked without touching the live default server: a wrapper with no
// socket name reports no file to unlink, so `gt down` and anything else killing
// through a default-socket wrapper cannot delete the town's socket path.
func TestKillServerLeavesTownSocketAlone(t *testing.T) {
	if (&Tmux{}).ownsSocketFile() {
		t.Fatal("default-socket wrapper would unlink the town socket file")
	}
	// And an unnamed wrapper's kill must not go looking for one either.
	dir := SocketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	(&Tmux{}).removeDeadSocketFile()
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(before) != len(after) {
		t.Errorf("socket dir changed: %d entries before, %d after", len(before), len(after))
	}
}

// TestUnlinkDeadSocketFileMissingPathIsQuiet pins the "already gone" path: a
// server that unlinked its own socket between the kill and the sweep is not an
// error, and no caller wants a second failure to report.
func TestUnlinkDeadSocketFileMissingPathIsQuiet(t *testing.T) {
	socket := constants.TestSocketName("gt-h9z-never-bound")
	socketPath := socketPathForTest(t, socket)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("pre-clean %s: %v", socketPath, err)
	}

	start := time.Now()
	unlinkDeadSocketFile(socketPath)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("unlink of a missing path took %s", elapsed)
	}
}
