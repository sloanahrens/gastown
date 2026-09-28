//go:build integration && !windows

package tmux

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/constants"
)

// These pin, against the real kernel and real tmux, each socket-directory
// case the unit tests script through fakeSockets.

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

func TestIntegrationUnlinkDeadSocketFile(t *testing.T) {
	ops := realSocketOps()
	t.Run("removes_stale_file", func(t *testing.T) {
		p := createStaleUnixSocket(t, uniqueSocketName(t, "gt-h9z-gone"))
		unlinkDeadSocketFile(clockwork.NewRealClock(), ops, p)
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("stale socket file survived: %v", err)
		}
	})
	t.Run("keeps_live_listener", func(t *testing.T) {
		l, p := listenOnSocketPath(t, uniqueSocketName(t, "gt-h9z-held"))
		go acceptAndClose(l)
		unlinkDeadSocketFile(clockwork.NewRealClock(), ops, p)
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("unlinked a socket with a live listener: %v", err)
		}
	})
	t.Run("leaves_plain_file", func(t *testing.T) {
		p := socketPathForTest(t, uniqueSocketName(t, "gt-h9z-plain"))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(p) })
		unlinkDeadSocketFile(clockwork.NewRealClock(), ops, p)
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("unlinked a file that was never a socket: %v", err)
		}
	})
	t.Run("missing_path", func(t *testing.T) {
		unlinkDeadSocketFile(clockwork.NewRealClock(), ops, socketPathForTest(t, uniqueSocketName(t, "gt-h9z-never")))
	})
}

func TestIntegrationEnsureNewSessionSocketSafe(t *testing.T) {
	guard := func(socket string) error { return NewTmuxWithSocket(socket).ensureNewSessionSocketSafe() }
	t.Run("stale", func(t *testing.T) {
		s := uniqueSocketName(t, "gt-h9z-stale")
		createStaleUnixSocket(t, s)
		if err := guard(s); err != nil {
			t.Fatalf("stale = %v", err)
		}
	})
	for _, tc := range []struct {
		name string
		make func(p string) error
		want string
	}{
		{"regular_file", func(p string) error { return os.WriteFile(p, []byte("x"), 0o600) }, "not a Unix socket"},
		{"directory", func(p string) error { return os.Mkdir(p, 0o700) }, "not a Unix socket"},
		{"symlink", func(p string) error { return os.Symlink(filepath.Join(filepath.Dir(p), "nowhere"), p) }, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := uniqueSocketName(t, "gt-h9z-"+tc.name)
			p := socketPathForTest(t, s)
			if err := tc.make(p); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(p) })
			if err := guard(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s = %v, want %q refusal", tc.name, err, tc.want)
			}
		})
	}
	t.Run("live_tmux_server", func(t *testing.T) {
		s := uniqueSocketName(t, "gt-h9z-live")
		tm := NewTmuxWithSocket(s)
		t.Cleanup(func() { _ = tm.KillServer() })
		if _, err := tm.run("new-session", "-d", "-s", "gt-h9z-live"); err != nil {
			t.Fatalf("new-session setup: %v", err)
		}
		if err := guard(s); err != nil {
			t.Fatalf("live tmux = %v", err)
		}
	})
	t.Run("live_listener_not_tmux", func(t *testing.T) {
		s := uniqueSocketName(t, "gt-h9z-notmux")
		l, _ := listenOnSocketPath(t, s)
		go acceptAndClose(l)
		if err := guard(s); err == nil {
			t.Fatal("non-tmux listener accepted")
		}
	})
}
