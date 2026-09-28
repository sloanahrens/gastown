//go:build !windows

package tmux

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/constants"
)

func uniqueSocketName(t *testing.T, prefix string) string {
	t.Helper()
	return constants.TestSocketName(prefix)
}

func socketPathForTest(t *testing.T, socket string) string {
	t.Helper()
	dir := SocketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	return filepath.Join(dir, socket)
}

func createStaleUnixSocket(t *testing.T, socket string) string {
	t.Helper()
	socketPath := socketPathForTest(t, socket)
	_ = os.Remove(socketPath)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix(%s): %v", socketPath, err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close stale listener: %v", err)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s mode = %s, want Unix socket", socketPath, info.Mode())
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	return socketPath
}

func listenOnSocketPath(t *testing.T, socket string) (*net.UnixListener, string) {
	t.Helper()
	socketPath := socketPathForTest(t, socket)
	_ = os.Remove(socketPath)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix(%s): %v", socketPath, err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
	return listener, socketPath
}

func assertSameSocketPath(t *testing.T, socketPath string, before os.FileInfo) {
	t.Helper()
	after, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat(%s) after refusal: %v", socketPath, err)
	}
	if after.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s after refusal mode = %s, want Unix socket", socketPath, after.Mode())
	}
	if !os.SameFile(before, after) {
		t.Fatalf("%s was replaced after refusal", socketPath)
	}
}

// guardTmux is a Tmux on socket whose tmux calls are scripted by s.
func guardTmux(socket string, s *scripted) *Tmux {
	return newTmuxForTest(socket, s.exec, clockwork.NewFakeClock())
}

func TestEnsureNewSessionSocketSafe(t *testing.T) {
	t.Parallel()
	t.Run("default_socket", func(t *testing.T) {
		if err := guardTmux("", newScripted(nil)).ensureNewSessionSocketSafe(); err != nil {
			t.Fatalf("ensureNewSessionSocketSafe(default) = %v", err)
		}
	})

	t.Run("absent_socket", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-absent")
		if err := guardTmux(socket, newScripted(nil)).ensureNewSessionSocketSafe(); err != nil {
			t.Fatalf("ensureNewSessionSocketSafe(absent) = %v", err)
		}
	})

	t.Run("stale_unix_socket", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-stale")
		createStaleUnixSocket(t, socket)
		s := newScripted(nil)
		if err := guardTmux(socket, s).ensureNewSessionSocketSafe(); err != nil {
			t.Fatalf("ensureNewSessionSocketSafe(stale) = %v", err)
		}
		if got := s.all(); len(got) != 0 {
			t.Errorf("probed a stale socket: %v", got)
		}
	})

	t.Run("regular_file", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-file")
		socketPath := socketPathForTest(t, socket)
		if err := os.WriteFile(socketPath, []byte("not a socket"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", socketPath, err)
		}
		t.Cleanup(func() { _ = os.Remove(socketPath) })

		err := guardTmux(socket, newScripted(nil)).ensureNewSessionSocketSafe()
		if err == nil {
			t.Fatal("ensureNewSessionSocketSafe(regular file) = nil, want error")
		}
		if !strings.Contains(err.Error(), socketPath) {
			t.Fatalf("error %q does not mention %s", err, socketPath)
		}
	})

	t.Run("directory", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-dir")
		socketPath := socketPathForTest(t, socket)
		if err := os.Mkdir(socketPath, 0o700); err != nil {
			t.Fatalf("Mkdir(%s): %v", socketPath, err)
		}
		t.Cleanup(func() { _ = os.Remove(socketPath) })

		if err := guardTmux(socket, newScripted(nil)).ensureNewSessionSocketSafe(); err == nil {
			t.Fatal("ensureNewSessionSocketSafe(directory) = nil, want error")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-link")
		socketPath := socketPathForTest(t, socket)
		target := socketPath + "-target"
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", target, err)
		}
		t.Cleanup(func() { _ = os.Remove(target) })
		if err := os.Symlink(target, socketPath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(socketPath) })

		err := guardTmux(socket, newScripted(nil)).ensureNewSessionSocketSafe()
		if err == nil {
			t.Fatal("ensureNewSessionSocketSafe(symlink) = nil, want error")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error %q should mention symlink", err)
		}
	})

	t.Run("live_tmux_server", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-live")
		listener, _ := listenOnSocketPath(t, socket)
		go acceptAndClose(listener)
		s := newScripted(nil) // list-sessions answers: a tmux server
		if err := guardTmux(socket, s).ensureNewSessionSocketSafe(); err != nil {
			t.Fatalf("ensureNewSessionSocketSafe(live tmux) = %v", err)
		}
		if probes := s.find("list-sessions"); len(probes) != 1 || probes[0].socket != socket {
			t.Fatalf("list-sessions probes = %v, want one on %s", probes, socket)
		}
	})

	t.Run("live_listener_not_tmux", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-notmux")
		listener, socketPath := listenOnSocketPath(t, socket)
		go acceptAndClose(listener)
		// The file answers a dial but tmux finds no server behind it: refuse,
		// because tmux would unlink and rebind a path something else owns.
		s := newScripted(bySub(map[string]reply{"list-sessions": fail("no server running on " + socketPath)}))
		err := guardTmux(socket, s).ensureNewSessionSocketSafe()
		if err == nil || !strings.Contains(err.Error(), "gt-h9z") {
			t.Fatalf("ensureNewSessionSocketSafe(non-tmux listener) = %v, want refusal", err)
		}
	})

	t.Run("live_listener_probe_error", func(t *testing.T) {
		socket := uniqueSocketName(t, "gt-h9z-probeerr")
		listener, _ := listenOnSocketPath(t, socket)
		go acceptAndClose(listener)
		s := newScripted(bySub(map[string]reply{"list-sessions": fail("protocol version mismatch")}))
		if err := guardTmux(socket, s).ensureNewSessionSocketSafe(); err == nil {
			t.Fatal("ensureNewSessionSocketSafe(probe error) = nil, want refusal")
		}
	})
}

// acceptAndClose accepts connections on l until it closes, closing each at
// once: enough for a dial to succeed.
func acceptAndClose(l *net.UnixListener) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

func TestNewSessionVariantsUseSocketGuard(t *testing.T) {
	t.Parallel()
	socket := uniqueSocketName(t, "gt-h9z-variants")
	socketPath := socketPathForTest(t, socket)
	if err := os.WriteFile(socketPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", socketPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	s := newScripted(nil)
	tm := guardTmux(socket, s)

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
			if !strings.Contains(err.Error(), socketPath) {
				t.Fatalf("error %q does not mention %s", err, socketPath)
			}
			if got := s.find("new-session"); len(got) != 0 {
				t.Fatalf("new-session sent past the guard: %v", got)
			}
		})
	}
}
