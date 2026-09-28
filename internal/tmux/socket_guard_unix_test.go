//go:build !windows

package tmux

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

const guardSocket = "gt-h9z"
const guardPath = "/fake-sock/" + guardSocket

// guardTmux is a Tmux on guardSocket whose socket directory is fs and whose
// tmux calls are scripted by s.
func guardTmux(fs *fakeSockets, s *scripted) *Tmux { return socketTmux(guardSocket, fs, s) }

func TestEnsureNewSessionSocketSafe(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, st sockState, s *scripted) error {
		t.Helper()
		fs := newFakeSockets()
		fs.set(guardPath, st)
		return guardTmux(fs, s).ensureNewSessionSocketSafe()
	}
	t.Run("default_socket", func(t *testing.T) {
		s := newScripted(nil)
		tm := newTmuxForTest("", s.exec, newFixedClock())
		tm.sock = newFakeSockets().ops()
		if err := tm.ensureNewSessionSocketSafe(); err != nil {
			t.Fatalf("ensureNewSessionSocketSafe(default) = %v", err)
		}
	})
	t.Run("absent_socket", func(t *testing.T) {
		if err := check(t, sockAbsent, newScripted(nil)); err != nil {
			t.Fatalf("absent = %v", err)
		}
	})
	t.Run("stale_unix_socket", func(t *testing.T) {
		s := newScripted(nil)
		if err := check(t, sockStale, s); err != nil {
			t.Fatalf("stale = %v", err)
		}
		if got := s.all(); len(got) != 0 {
			t.Errorf("probed a stale socket: %v", got)
		}
	})
	for _, tc := range []struct {
		name string
		st   sockState
		want string
	}{
		{"regular_file", sockFile, "not a Unix socket"},
		{"directory", sockDir, "not a Unix socket"},
		{"symlink", sockSymlink, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := check(t, tc.st, newScripted(nil))
			if err == nil || !strings.Contains(err.Error(), guardPath) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s = %v, want a refusal naming %s and %q", tc.name, err, guardPath, tc.want)
			}
		})
	}
	t.Run("live_tmux_server", func(t *testing.T) {
		s := newScripted(nil) // list-sessions answers: a tmux server
		if err := check(t, sockLive, s); err != nil {
			t.Fatalf("live tmux = %v", err)
		}
		if probes := s.find("list-sessions"); len(probes) != 1 || probes[0].socket != guardSocket {
			t.Fatalf("list-sessions probes = %v, want one on %s", probes, guardSocket)
		}
	})
	t.Run("live_listener_not_tmux", func(t *testing.T) {
		// The file answers a dial but tmux finds no server behind it: refuse,
		// because tmux would unlink and rebind a path something else owns.
		s := newScripted(bySub(map[string]reply{"list-sessions": fail("no server running on " + guardPath)}))
		if err := check(t, sockLive, s); err == nil || !strings.Contains(err.Error(), "gt-h9z") {
			t.Fatalf("non-tmux listener = %v, want refusal", err)
		}
	})
	t.Run("live_listener_goes_stale_during_probe", func(t *testing.T) {
		fs := newFakeSockets()
		fs.set(guardPath, sockStale)
		fs.queueDial(guardPath, nil) // live at the first check only
		s := newScripted(bySub(map[string]reply{"list-sessions": fail("no server running on " + guardPath)}))
		if err := guardTmux(fs, s).ensureNewSessionSocketSafe(); err != nil {
			t.Fatalf("server that exited during the probe = %v, want nil (the file is stale now)", err)
		}
	})
	t.Run("live_listener_probe_error", func(t *testing.T) {
		s := newScripted(bySub(map[string]reply{"list-sessions": fail("protocol version mismatch")}))
		if err := check(t, sockLive, s); err == nil {
			t.Fatal("probe error = nil, want refusal")
		}
	})
	t.Run("dial_error", func(t *testing.T) {
		fs := newFakeSockets()
		fs.set(guardPath, sockStale)
		fs.queueDial(guardPath, &os.SyscallError{Syscall: "connect", Err: syscall.EACCES})
		err := guardTmux(fs, newScripted(nil)).ensureNewSessionSocketSafe()
		if err == nil || !strings.Contains(err.Error(), "cannot be safely contacted") {
			t.Fatalf("dial error = %v, want refusal", err)
		}
	})
}

func TestNewSessionVariantsUseSocketGuard(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set(guardPath, sockFile)
	s := newScripted(nil)
	tm := guardTmux(fs, s)

	tests := []struct {
		name string
		run  func(string) error
	}{
		{name: "NewSession", run: func(name string) error { return tm.NewSession(name, "") }},
		{name: "NewSessionWithCommand", run: func(name string) error { return tm.NewSessionWithCommand(name, "", "sleep 5") }},
		{name: "NewSessionWithCommandAndEnv", run: func(name string) error {
			return tm.NewSessionWithCommandAndEnv(name, "", "sleep 5", map[string]string{"GT_TEST": "1"})
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(fmt.Sprintf("gt-h9z-variant-%d", i))
			if err == nil {
				t.Fatal("creation variant returned nil, want socket guard error")
			}
			if !strings.Contains(err.Error(), guardPath) {
				t.Fatalf("error %q does not mention %s", err, guardPath)
			}
			if got := s.find("new-session"); len(got) != 0 {
				t.Fatalf("new-session sent past the guard: %v", got)
			}
		})
	}
}
