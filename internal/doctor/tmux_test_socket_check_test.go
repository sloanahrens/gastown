package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func newTestSocketDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("writing socket file %s: %v", name, err)
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
	check.socketDirForTest = newTestSocketDir(t, "gt-test-91506")
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
		{"gt-3aa519", 0, false},
		{"gt-test-sentinel", 0, false},
		{"default", 0, false},
	}
	for _, tc := range cases {
		got, ok := candidateOwnerPid(tc.socket)
		if ok != tc.ok || got != tc.want {
			t.Errorf("candidateOwnerPid(%q) = (%d, %v), want (%d, %v)", tc.socket, got, ok, tc.want, tc.ok)
		}
	}
}
