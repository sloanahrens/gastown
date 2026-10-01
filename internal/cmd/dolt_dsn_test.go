package cmd

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// mockSocket answers every port with sockPath, as a live local Dolt socket.
func mockSocket(sockPath string) func(int) string { return func(int) string { return sockPath } }

// noSocket answers that no local Dolt socket is live, so tests assert the TCP
// fallback on machines that happen to have Dolt running locally.
func noSocket(int) string { return "" }

func TestBuildDoltDSN_Socket(t *testing.T) {
	t.Parallel()
	got := buildDoltDSNVia(mockSocket("/tmp/mysql.sock"), "root", 3307, "hq", dsnOpts{
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
	t.Parallel()
	got := buildDoltDSNVia(noSocket, "root", 3307, "hq", dsnOpts{
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
	t.Parallel()
	// install.go:522 uses no dbName; the trailing slash with empty dbName
	// is valid in the go-mysql-driver DSN spec.
	got := buildDoltDSNVia(noSocket, "root", 3307, "", dsnOpts{
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
	t.Parallel()
	// install.go:662 has no query parameters; helper should omit the
	// trailing "?".
	got := buildDoltDSNVia(noSocket, "root", 3307, "", dsnOpts{})
	want := "root@tcp(127.0.0.1:3307)/"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSN_DefaultUser(t *testing.T) {
	t.Parallel()
	// Empty user defaults to "root" (matches the inline DSNs that
	// previously hardcoded "root").
	got := buildDoltDSNVia(noSocket, "", 3307, "hq", dsnOpts{})
	if !strings.HasPrefix(got, "root@") {
		t.Errorf("expected DSN to start with root@, got %q", got)
	}
}

func TestBuildDoltDSN_AllOpts(t *testing.T) {
	t.Parallel()
	// Every option populated → all four query params present in declared order.
	got := buildDoltDSNVia(noSocket, "root", 3307, "hq", dsnOpts{
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
	t.Parallel()
	cfg := &doltserver.Config{User: "root", Port: 3307}
	got := buildDoltDSNFromConfigVia(noSocket, cfg, "hq", dsnOpts{
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
	t.Parallel()
	cfg := &doltserver.Config{User: "alice", Host: "10.0.0.5", Port: 13306}
	got := buildDoltDSNFromConfigVia(mockSocket("/tmp/mysql.13306.sock"), cfg, "hq", dsnOpts{ParseTime: true})
	want := "alice@tcp(10.0.0.5:13306)/hq?parseTime=true"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestBuildDoltDSNFromConfig_LocalHostFallbackPreserved(t *testing.T) {
	t.Parallel()
	cfg := &doltserver.Config{User: "root", Host: "localhost", Port: 13306}
	got := buildDoltDSNFromConfigVia(noSocket, cfg, "hq", dsnOpts{})
	want := "root@tcp(localhost:13306)/hq"
	if got != want {
		t.Errorf("got\n  %s\nwant\n  %s", got, want)
	}
}

// countingDialer is a doltSocketDialer that hands each 1-based attempt to
// fn and counts the attempts.
func countingDialer(fn func(attempt int) (net.Conn, error)) (doltSocketDialer, *int) {
	var attempts int
	return func(network, addr string, timeout time.Duration) (net.Conn, error) {
		attempts++
		return fn(attempts)
	}, &attempts
}

// stubConn is the connection a successful fake dial returns.
type stubConn struct{ net.Conn }

func (stubConn) Close() error { return nil }

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
	t.Parallel()
	sockPath := "/tmp/gt-retry.sock"
	dial, attempts := countingDialer(func(attempt int) (net.Conn, error) {
		if attempt < doltSocketDialAttempts {
			return nil, dialTimeoutErr{}
		}
		return stubConn{}, nil
	})

	got := dialDoltSocket(dial, sockPath)
	if got != sockPath {
		t.Fatalf("dialDoltSocket returned \"\" after %d dial attempts; a timed-out "+
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
	t.Parallel()
	dial, attempts := countingDialer(func(int) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: errors.New("connection refused")}
	})

	if got := dialDoltSocket(dial, "/tmp/gt-refused.sock"); got != "" {
		t.Errorf("dialDoltSocket = %q, want \"\" for a refused socket", got)
	}
	if *attempts != 1 {
		t.Errorf("dial attempts = %d, want 1 (a refused connect is not a timeout)", *attempts)
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
