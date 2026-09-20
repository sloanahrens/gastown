package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// withMockSocket replaces localDoltSocketPath for the duration of the
// test with a function that returns sockPath unconditionally. Useful for
// asserting the unix-socket DSN branch without requiring a real Dolt.
func withMockSocket(t *testing.T, sockPath string) {
	t.Helper()
	orig := localDoltSocketPath
	localDoltSocketPath = func(int) string { return sockPath }
	t.Cleanup(func() { localDoltSocketPath = orig })
}

// withNoSocket forces localDoltSocketPath to return "" so tests can
// assert the TCP fallback branch on machines that happen to have Dolt
// running locally.
func withNoSocket(t *testing.T) {
	t.Helper()
	orig := localDoltSocketPath
	localDoltSocketPath = func(int) string { return "" }
	t.Cleanup(func() { localDoltSocketPath = orig })
}

func TestBuildDoltDSN_Socket(t *testing.T) {
	withMockSocket(t, "/tmp/mysql.sock")
	got := buildDoltDSN("root", 3307, "hq", dsnOpts{
		ParseTime:   true,
		Timeout:     "5s",
		ReadTimeout: "10s",
	})
	want := "root@unix(/tmp/mysql.sock)/hq?parseTime=true&timeout=5s&readTimeout=10s"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSN_TCPFallback(t *testing.T) {
	withNoSocket(t)
	got := buildDoltDSN("root", 3307, "hq", dsnOpts{
		ParseTime:   true,
		Timeout:     "5s",
		ReadTimeout: "10s",
	})
	want := "root@tcp(127.0.0.1:3307)/hq?parseTime=true&timeout=5s&readTimeout=10s"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSN_EmptyDBName(t *testing.T) {
	// install.go:522 uses no dbName; the trailing slash with empty dbName
	// is valid in the go-mysql-driver DSN spec.
	withNoSocket(t)
	got := buildDoltDSN("root", 3307, "", dsnOpts{
		Timeout:      "1s",
		ReadTimeout:  "1s",
		WriteTimeout: "1s",
	})
	want := "root@tcp(127.0.0.1:3307)/?timeout=1s&readTimeout=1s&writeTimeout=1s"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSN_NoQueryParams(t *testing.T) {
	// install.go:662 has no query parameters; helper should omit the
	// trailing "?".
	withNoSocket(t)
	got := buildDoltDSN("root", 3307, "", dsnOpts{})
	want := "root@tcp(127.0.0.1:3307)/"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSN_DefaultUser(t *testing.T) {
	// Empty user defaults to "root" (matches the inline DSNs that
	// previously hardcoded "root").
	withNoSocket(t)
	got := buildDoltDSN("", 3307, "hq", dsnOpts{})
	if !strings.HasPrefix(got, "root@") {
		t.Errorf("expected DSN to start with root@, got %q", got)
	}
}

func TestBuildDoltDSN_AllOpts(t *testing.T) {
	// Every option populated → all four query params present in declared order.
	withNoSocket(t)
	got := buildDoltDSN("root", 3307, "hq", dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "30s",
		WriteTimeout: "30s",
	})
	want := "root@tcp(127.0.0.1:3307)/hq?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSNFromConfig(t *testing.T) {
	withNoSocket(t)
	cfg := &doltserver.Config{User: "root", Port: 3307}
	got := buildDoltDSNFromConfig(cfg, "hq", dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "30s",
		WriteTimeout: "30s",
	})
	want := "root@tcp(127.0.0.1:3307)/hq?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSNFromConfig_RemoteHostPreserved(t *testing.T) {
	withMockSocket(t, "/tmp/mysql.13306.sock")
	cfg := &doltserver.Config{User: "alice", Host: "10.0.0.5", Port: 13306}
	got := buildDoltDSNFromConfig(cfg, "hq", dsnOpts{ParseTime: true})
	want := "alice@tcp(10.0.0.5:13306)/hq?parseTime=true"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSNFromConfig_LocalHostFallbackPreserved(t *testing.T) {
	withNoSocket(t)
	cfg := &doltserver.Config{User: "root", Host: "localhost", Port: 13306}
	got := buildDoltDSNFromConfig(cfg, "hq", dsnOpts{})
	want := "root@tcp(localhost:13306)/hq"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

// TestLocalDoltSocketPath_RealSocket verifies the actual probe (not the
// test mock) recognizes a live unix socket.
func TestLocalDoltSocketPath_RealSocket(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix domain sockets not supported on Windows")
	}

	port := 20000 + os.Getpid()%10000
	sockPath := fmt.Sprintf("/tmp/mysql.%d.sock", port)
	_ = os.Remove(sockPath)
	t.Cleanup(func() { _ = os.Remove(sockPath) })

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// Drain connections the way a real Dolt server does. The probe retries a
	// connect whose deadline expired while this process was descheduled; with
	// no accept loop those attempts pile up unread in the listen backlog, and
	// the backlog is bounded — a full one turns a retry into a hang
	// (gt-hvzy.3 / gt-bzkt).
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	got := buildDoltDSN("root", port, "hq", dsnOpts{Timeout: "1s"})
	wantSubstr := "@unix(" + sockPath + ")/hq"
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("got %q; expected to contain %q", got, wantSubstr)
	}
}

func TestLocalDoltSocketPath_StaleSocketReturnsEmpty(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix domain sockets not supported on Windows")
	}

	port := 30000 + os.Getpid()%10000
	sockPath := fmt.Sprintf("/tmp/mysql.%d.sock", port)
	_ = os.Remove(sockPath)
	t.Cleanup(func() { _ = os.Remove(sockPath) })

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	got := buildDoltDSN("root", port, "hq", dsnOpts{Timeout: "1s"})
	wantSubstr := fmt.Sprintf("@tcp(127.0.0.1:%d)/hq", port)
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("expected TCP fallback for stale socket, got %q", got)
	}
}

func TestLocalDoltSocketPath_AbsentReturnsEmpty(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "wad6f")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	nonExistent := filepath.Join(tmpDir, "not-a-real-socket.sock")
	orig := localDoltSocketPath
	localDoltSocketPath = func(int) string {
		info, err := os.Stat(nonExistent)
		if err != nil {
			return ""
		}
		if info.Mode()&os.ModeSocket == 0 {
			return ""
		}
		return nonExistent
	}
	t.Cleanup(func() { localDoltSocketPath = orig })

	got := buildDoltDSN("root", 3307, "hq", dsnOpts{Timeout: "1s"})
	if !strings.Contains(got, "@tcp(127.0.0.1:3307)/hq") {
		t.Errorf("expected TCP fallback when socket absent, got %q", got)
	}
}

// shortSockPath returns a unix socket path that fits macOS's 104-byte
// sun_path limit — t.TempDir() resolves under /var/folders and is far too
// long to bind. Cleaned up via t.Cleanup.
func shortSockPath(t *testing.T, tag string) string {
	t.Helper()
	p := fmt.Sprintf("/tmp/gt-%s-%d.sock", tag, os.Getpid())
	_ = os.Remove(p)
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

// socketPathFixture plants a live unix socket at a short /tmp path and returns
// it, so the retry tests exercise the real probe body (stat + mode check + dial
// loop) without depending on /tmp/mysql.* being free or on a real Dolt server
// being up. The accept loop mirrors a real Dolt server: without it, retried
// connects pile up unread in the bounded listen backlog.
func socketPathFixture(t *testing.T, tag string) string {
	t.Helper()
	sockPath := shortSockPath(t, tag)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return sockPath
}

// swapDoltSocketDial replaces the dial seam for the duration of the test and
// returns a pointer to the attempt counter it increments. fn receives the
// 1-based attempt number so it can fail for the first N attempts.
func swapDoltSocketDial(t *testing.T, fn func(attempt int, network, addr string, timeout time.Duration) (net.Conn, error)) *int {
	t.Helper()
	var attempts int
	orig := doltSocketDial
	doltSocketDial = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		attempts++
		return fn(attempts, network, addr, timeout)
	}
	t.Cleanup(func() { doltSocketDial = orig })
	return &attempts
}

// dialTimeoutErr is a net.Error reporting Timeout() == true, matching what
// net.DialTimeout returns when the deadline expires.
type dialTimeoutErr struct{}

func (dialTimeoutErr) Error() string   { return "i/o timeout" }
func (dialTimeoutErr) Timeout() bool   { return true }
func (dialTimeoutErr) Temporary() bool { return true }

// TestLocalDoltSocketPath_RetriesDialTimeout pins the bound: a live socket
// whose connect times out because this process was descheduled must be
// retried, not silently demoted to TCP. This is the load-sensitive flake from
// gt-bzkt (TestLocalDoltSocketPath_RealSocket at host load 24.7) and also the
// production bug — a loaded host makes gt choose tcp over the socket and pay a
// TIME_WAIT entry per close, the thing the socket transport exists to avoid.
// Asserting the attempt count here is deterministic; asserting it via a real
// starved dial is not.
func TestLocalDoltSocketPath_RetriesDialTimeout(t *testing.T) {
	sockPath := socketPathFixture(t, "retry")

	origDial := doltSocketDial
	attempts := swapDoltSocketDial(t, func(attempt int, network, addr string, timeout time.Duration) (net.Conn, error) {
		if attempt < doltSocketDialAttempts {
			return nil, dialTimeoutErr{}
		}
		return origDial(network, addr, timeout)
	})

	got := probeDoltSocket(sockPath)
	if got == "" {
		t.Fatalf("probeDoltSocket returned \"\" after %d dial attempts; a timed-out "+
			"connect must be retried, not treated as a stale socket", *attempts)
	}
	if *attempts != doltSocketDialAttempts {
		t.Errorf("dial attempts = %d, want %d", *attempts, doltSocketDialAttempts)
	}
}

// TestLocalDoltSocketPath_RefusedIsNotRetried is the other half of the bound:
// a stale socket file is refused immediately, so the probe must give up on the
// first attempt rather than burning doltSocketDialAttempts timeouts. Without
// this, every DSN build on a box with a leftover socket file pays the full
// retry budget.
func TestLocalDoltSocketPath_RefusedIsNotRetried(t *testing.T) {
	sockPath := socketPathFixture(t, "refused")

	attempts := swapDoltSocketDial(t, func(attempt int, network, addr string, timeout time.Duration) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: errors.New("connection refused")}
	})

	if got := probeDoltSocket(sockPath); got != "" {
		t.Errorf("probeDoltSocket = %q, want \"\" for a refused socket", got)
	}
	if *attempts != 1 {
		t.Errorf("dial attempts = %d, want 1 (a refused connect is not a timeout)", *attempts)
	}
}

// TestLocalDoltSocketPath_StaleSocketIsNotRetried checks the same bound through
// the real dialer: a socket file whose listener has gone away refuses the
// connect, and that ECONNREFUSED must short-circuit the retry loop.
func TestLocalDoltSocketPath_StaleSocketIsNotRetried(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix domain sockets not supported on Windows")
	}
	sockPath := shortSockPath(t, "stale")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.UnixListener", listener)
	}
	// Go unlinks the socket file on Close by default, which would leave the
	// probe a missing file rather than a refused connect. Keep the file so
	// this actually exercises the ECONNREFUSED path.
	unixListener.SetUnlinkOnClose(false)

	attempts := swapDoltSocketDial(t, func(attempt int, network, addr string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout(network, addr, timeout)
	})

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	if got := probeDoltSocket(sockPath); got != "" {
		t.Errorf("probeDoltSocket = %q, want \"\" for a stale socket file", got)
	}
	if *attempts != 1 {
		t.Errorf("dial attempts = %d, want 1 (ECONNREFUSED is not a timeout)", *attempts)
	}
}

// TestDoltSocketPathForPort pins the path derivation the probe wrapper uses.
func TestDoltSocketPathForPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		port int
		want string
	}{
		{0, "/tmp/mysql.sock"},
		{3306, "/tmp/mysql.sock"},
		{3307, "/tmp/mysql.3307.sock"},
		{13306, "/tmp/mysql.13306.sock"},
	}
	for _, tt := range tests {
		if got := doltSocketPathForPort(tt.port); got != tt.want {
			t.Errorf("doltSocketPathForPort(%d) = %q, want %q", tt.port, got, tt.want)
		}
	}
}
