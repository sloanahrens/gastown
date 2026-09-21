package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTestSocketProbe stands in for the tmux client on one socket.
type fakeTestSocketProbe struct {
	sessions []string
	listErr  error
	killed   bool
}

func (f *fakeTestSocketProbe) ListSessions() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.sessions, nil
}

func (f *fakeTestSocketProbe) GetPanePID(string) (string, error) { return "4242", nil }

func (f *fakeTestSocketProbe) PaneCurrentPath(string) (string, error) {
	return "/private/var/folders/xy/T", nil
}

func (f *fakeTestSocketProbe) KillServer() error {
	f.killed = true
	return nil
}

// newTestSocketDir builds a socket directory holding one socket file per name.
// The files are fresh, which is what a server bound moments ago looks like.
func newTestSocketDir(t *testing.T, names ...string) string {
	t.Helper()
	return newAgedSocketDir(t, 0, names...)
}

// newAgedSocketDir builds a socket directory whose files are all age old. The
// mtime is what dates a socket file: tmux sets it when it binds and never
// touches it again, so an old file is one whose run is long over.
func newAgedSocketDir(t *testing.T, age time.Duration, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("writing socket file %s: %v", name, err)
		}
		if age > 0 {
			when := time.Now().Add(-age)
			if err := os.Chtimes(path, when, when); err != nil {
				t.Fatalf("aging socket file %s: %v", name, err)
			}
		}
	}
	return dir
}

func TestTmuxTestSocketCheck_NoSockets(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t)
	check.pidAliveForTest = func(int) bool { return true }

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Errorf("status = %v, want OK: %s", result.Status, result.Message)
	}
}

// TestTmuxTestSocketCheck_ReportsLeftover is the gt-2bj tripwire: a socket whose
// owning test process is gone still holding gt-test-* sessions.
func TestTmuxTestSocketCheck_ReportsLeftover(t *testing.T) {
	probe := &fakeTestSocketProbe{sessions: []string{"gt-test-modeA-2", "gt-test-sentinel"}}
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe { return probe }

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", result.Status, result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"gt-test-91506", "owner pid 91506 is gone", "gt-test-modeA-2",
		"pane_pid=4242", "cwd=/private/var/folders/xy/T",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("details missing %q:\n%s", want, joined)
		}
	}
}

// TestTmuxTestSocketCheck_StaleFileIsNotAServer covers the other residue a
// killed run leaves: the socket file survives its server. It is not evidence of
// a phantom, and it is counted only because cleaning it keeps the scan cheap.
func TestTmuxTestSocketCheck_StaleFileIsNotAServer(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newAgedSocketDir(t, time.Hour, "gt-test-91506")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return false }
	check.probeForTest = func(string) testSocketProbe {
		t.Error("probed a socket with no server")
		return &fakeTestSocketProbe{}
	}

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "1 socket file(s)") {
		t.Errorf("message = %q, want the stale-file count", result.Message)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "phantom") {
		t.Errorf("a serverless socket must not be reported as a phantom: %v", result.Details)
	}
}

// TestTmuxTestSocketCheck_FreshFileIsLeftAlone covers the bind window: a file
// exists for a moment before its server is listening, so a sweep that trusted
// "nothing answers" alone could unlink a socket out from under a new server.
func TestTmuxTestSocketCheck_FreshFileIsLeftAlone(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
	check.servingForTest = func(string) bool { return false }
	check.probeForTest = func(string) testSocketProbe {
		t.Error("probed a socket with no server")
		return &fakeTestSocketProbe{}
	}

	if result := check.Run(&CheckContext{TownRoot: t.TempDir()}); result.Status != StatusOK {
		t.Errorf("status = %v, want OK for a file younger than the sweep age: %s",
			result.Status, result.Message)
	}
}

// TestTmuxTestSocketCheck_StaleFileWithoutOwnerIsCollected is the gt-20di litter
// fix: a file's owner pid is unrecoverable from a name that does not carry one
// (gt-test-sentinel), and from one whose pid has since been recycled onto an
// unrelated live process. Neither socket has a server, so neither is anyone's to
// keep.
func TestTmuxTestSocketCheck_StaleFileWithoutOwnerIsCollected(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newAgedSocketDir(t, time.Hour, "gt-test-sentinel", "gt-test-91506")
	// The pid in the second name is alive — it just is not this socket's owner.
	check.pidAliveForTest = func(int) bool { return true }
	check.servingForTest = func(string) bool { return false }
	check.probeForTest = func(string) testSocketProbe {
		t.Error("probed a socket with no server")
		return &fakeTestSocketProbe{}
	}

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "2 socket file(s)") {
		t.Errorf("message = %q, want both unserved files counted", result.Message)
	}
}

// TestTmuxTestSocketCheck_ForeignSocketIsUntouched guards the town socket: the
// check's directory is the same one every real server binds in, so a name
// outside the test families must never be collected, served or not.
func TestTmuxTestSocketCheck_ForeignSocketIsUntouched(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newAgedSocketDir(t, time.Hour, "gt-3aa519", "default")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return false }
	check.probeForTest = func(string) testSocketProbe {
		t.Error("probed a socket outside the test families")
		return &fakeTestSocketProbe{}
	}

	if result := check.Run(&CheckContext{TownRoot: t.TempDir()}); result.Status != StatusOK {
		t.Errorf("status = %v, want OK — a foreign socket is never ours: %s",
			result.Status, result.Message)
	}
}

// TestTmuxTestSocketCheck_H9zFamilyIsOurs covers the second test family: the
// socket-guard tests bind gt-h9z-* servers, and their sessions are named after
// the guard rather than after the test, so both halves of the check have to
// recognize the family.
func TestTmuxTestSocketCheck_H9zFamilyIsOurs(t *testing.T) {
	probe := &fakeTestSocketProbe{sessions: []string{"gt-h9z-live"}}
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-h9z-live-91506")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe { return probe }

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "abandoned test tmux server") {
		t.Errorf("message = %q, want a leftover server report", result.Message)
	}
}

// TestTmuxTestSocketCheck_TimestampShapedOwnerIsNotAPid is the safety half of
// the family widening: a socket whose trailing field is a nanosecond stamp (the
// shape the check used to misread as an always-dead pid) does not name its
// owner, so its live server is left running rather than killed under a test
// that may still be using it.
func TestTmuxTestSocketCheck_TimestampShapedOwnerIsNotAPid(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-dog-stale-1758012345678901234")
	check.pidAliveForTest = func(int) bool {
		t.Error("read a timestamp as a pid")
		return false
	}
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe {
		t.Error("probed a socket whose owner cannot be identified")
		return &fakeTestSocketProbe{sessions: []string{"gt-test-dog-stale"}}
	}

	if result := check.Run(&CheckContext{TownRoot: t.TempDir()}); result.Status != StatusOK {
		t.Errorf("status = %v, want OK — an unattributable server is not ours to kill: %s",
			result.Status, result.Message)
	}
}

// TestTmuxTestSocketCheck_LiveOwnerIsNotALeak keeps a running suite safe: the
// socket of a test process that is still alive belongs to work in flight.
func TestTmuxTestSocketCheck_LiveOwnerIsNotALeak(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
	check.pidAliveForTest = func(int) bool { return true }
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe {
		t.Error("probed a socket whose owner is alive")
		return &fakeTestSocketProbe{}
	}

	if result := check.Run(&CheckContext{TownRoot: t.TempDir()}); result.Status != StatusOK {
		t.Errorf("status = %v, want OK: %s", result.Status, result.Message)
	}
}

// TestTmuxTestSocketCheck_ForeignSessionsAreNotOurs stops the Fix from killing a
// server that is serving something other than test sessions.
func TestTmuxTestSocketCheck_ForeignSessionsAreNotOurs(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe {
		return &fakeTestSocketProbe{sessions: []string{"gt-garnet"}}
	}

	if result := check.Run(&CheckContext{TownRoot: t.TempDir()}); result.Status != StatusOK {
		t.Errorf("status = %v, want OK: %s", result.Status, result.Message)
	}
}

// TestTmuxTestSocketCheck_ServerVanishedMidScan covers the race the dial cannot
// close: the socket answered, then its server exited before the sessions were
// listed. The socket file is then residue, not evidence.
func TestTmuxTestSocketCheck_ServerVanishedMidScan(t *testing.T) {
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe {
		return &fakeTestSocketProbe{listErr: errors.New("no server running")}
	}

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %s", result.Status, result.Message)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "phantom") {
		t.Errorf("a serverless socket must not be reported as a phantom: %v", result.Details)
	}
}

func TestTmuxTestSocketCheck_FixKillsLeftover(t *testing.T) {
	probe := &fakeTestSocketProbe{sessions: []string{"gt-test-modeA-2"}}
	check := NewTmuxTestSocketCheck()
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
	check.pidAliveForTest = func(int) bool { return false }
	check.servingForTest = func(string) bool { return true }
	check.probeForTest = func(string) testSocketProbe { return probe }

	ctx := &CheckContext{TownRoot: t.TempDir()}
	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("setup: status = %v, want warning", result.Status)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !probe.killed {
		t.Error("Fix did not kill the abandoned server")
	}
	if _, err := os.Stat(filepath.Join(check.socketDirForTest, "gt-test-91506")); !os.IsNotExist(err) {
		t.Errorf("socket file still present after Fix: %v", err)
	}
}

func TestCandidateOwnerPid(t *testing.T) {
	cases := []struct {
		socket string
		want   int
		ok     bool
	}{
		{"gt-test-91506", 91506, true},
		{"gt-test-daemon-20703", 20703, true},
		{"gt-test-config-4242", 4242, true},
		{"gt-h9z-live-1758012345678901234-4242", 4242, true},
		{"gt-3aa519", 0, false},
		{"gt-test-sentinel", 0, false},
		{"default", 0, false},
		// A timestamp in the trailing position is not a pid, and reading one as
		// a pid is what made the check see every such socket's owner as dead.
		{"gt-test-dog-stale-1758012345678901234", 0, false},
	}
	for _, tc := range cases {
		got, ok := candidateOwnerPid(tc.socket)
		if ok != tc.ok || got != tc.want {
			t.Errorf("candidateOwnerPid(%q) = (%d, %v), want (%d, %v)", tc.socket, got, ok, tc.want, tc.ok)
		}
	}
}
