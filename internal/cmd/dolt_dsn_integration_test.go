//go:build integration

package cmd

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

// TestIntegrationDoltSocketProbe drives the real probe against real unix
// sockets: a live one is used, one whose listener is gone is refused on the
// first connect and falls back to TCP, and a missing one falls back to TCP.
// The retry policy itself is unit-tested on dialDoltSocket.
func TestIntegrationDoltSocketProbe(t *testing.T) {
	// /tmp, not t.TempDir(): macOS caps sun_path at 104 bytes and
	// t.TempDir() resolves far deeper under /var/folders.
	sockPath := func(tag string) string {
		p := fmt.Sprintf("/tmp/gt-%s-%d.sock", tag, os.Getpid())
		_ = os.Remove(p)
		t.Cleanup(func() { _ = os.Remove(p) })
		return p
	}

	t.Run("live socket", func(t *testing.T) {
		p := sockPath("live")
		listener, err := net.Listen("unix", p)
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		// Drain connections the way a real Dolt server does: a retried connect
		// left unread in the bounded listen backlog turns into a hang.
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
		if got := probeDoltSocket(p); got != p {
			t.Errorf("probeDoltSocket(live) = %q, want %q", got, p)
		}
		got := buildDoltDSNVia(func(int) string { return probeDoltSocket(p) }, "root", 3307, "hq", dsnOpts{Timeout: "1s"})
		if !strings.Contains(got, "@unix("+p+")/hq") {
			t.Errorf("DSN = %q, want the live socket", got)
		}
	})

	t.Run("stale socket file", func(t *testing.T) {
		p := sockPath("stale")
		listener, err := net.Listen("unix", p)
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		// Go unlinks the socket file on Close by default, which would leave
		// the probe a missing file rather than a refused connect.
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
		if got := probeDoltSocket(p); got != "" {
			t.Errorf("probeDoltSocket(stale) = %q, want \"\"", got)
		}
	})

	t.Run("absent socket", func(t *testing.T) {
		if got := probeDoltSocket(sockPath("absent")); got != "" {
			t.Errorf("probeDoltSocket(absent) = %q, want \"\"", got)
		}
	})
}
