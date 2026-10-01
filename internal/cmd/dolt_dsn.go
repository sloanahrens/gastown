package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// dsnOpts captures the optional MySQL DSN query parameters used by gt's
// internal Dolt-server connections. Empty / zero values are omitted from
// the resulting query string.
type dsnOpts struct {
	ParseTime    bool
	Timeout      string // e.g. "5s"
	ReadTimeout  string // e.g. "10s"
	WriteTimeout string // e.g. "30s"
}

func (o dsnOpts) queryString() string {
	var parts []string
	if o.ParseTime {
		parts = append(parts, "parseTime=true")
	}
	if o.Timeout != "" {
		parts = append(parts, "timeout="+o.Timeout)
	}
	if o.ReadTimeout != "" {
		parts = append(parts, "readTimeout="+o.ReadTimeout)
	}
	if o.WriteTimeout != "" {
		parts = append(parts, "writeTimeout="+o.WriteTimeout)
	}
	return strings.Join(parts, "&")
}

// doltSocketDialTimeout bounds a single connect attempt against the socket,
// and doltSocketDialAttempts bounds how many times a *timed out* attempt is
// retried. A live unix socket accepts in microseconds — a timeout means this
// process was descheduled, not that Dolt is absent. Retrying gives the
// scheduler another chance instead of silently demoting a live socket to TCP
// on a loaded host, which is itself a production bug: the caller then pays a
// TIME_WAIT entry per close, the very thing the socket transport exists to
// avoid (gt-hvzy.3 / gt-bzkt).
//
// Only timeouts are retried. ECONNREFUSED / ENOENT mean the socket file is
// stale, which is the common case on a box with no Dolt running; those return
// immediately so the probe stays cheap.
const (
	doltSocketDialTimeout  = 100 * time.Millisecond
	doltSocketDialAttempts = 3
)

// doltSocketDialer performs one connect; net.DialTimeout in production.
type doltSocketDialer func(network, addr string, timeout time.Duration) (net.Conn, error)

// doltSocketPathForPort derives Dolt's default unix socket path for a port.
// Mirrors the path-derivation logic already in this package (see
// internal/doltserver/doltserver.go cleanStaleDoltSocket): Dolt listens on
// /tmp/mysql.sock on port 3306, /tmp/mysql.{port}.sock for any other port.
func doltSocketPathForPort(port int) string {
	if port == 0 || port == 3306 {
		return "/tmp/mysql.sock"
	}
	return fmt.Sprintf("/tmp/mysql.%d.sock", port)
}

// probeDoltSocket returns p (the path of a live unix socket accepting
// connections) or "" if nothing is listening there.
//
// Separated from path derivation so tests can point the probe at a temp-dir
// socket directly, without a real Dolt server.
func probeDoltSocket(p string) string {
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	if info.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return dialDoltSocket(net.DialTimeout, p)
}

// dialDoltSocket returns p once dial connects to it, retrying a connect that
// timed out, or "" once one is refused or the attempts run out. The dialer is
// a parameter so the retry policy can be asserted deterministically (attempt
// count on timeout vs. refusal) without inducing real scheduler starvation in
// a test.
func dialDoltSocket(dial doltSocketDialer, p string) string {
	for attempt := 0; attempt < doltSocketDialAttempts; attempt++ {
		conn, err := dial("unix", p, doltSocketDialTimeout)
		if err == nil {
			_ = conn.Close()
			return p
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			// Definitively refused or gone: the socket file is stale.
			// Retrying cannot help, and the caller must fall back to TCP.
			return ""
		}
	}
	return ""
}

// localDoltSocketPath returns Dolt's default unix socket path for a given
// port if a unix socket is currently accepting connections at that path;
// otherwise returns "".
func localDoltSocketPath(port int) string {
	return probeDoltSocket(doltSocketPathForPort(port))
}

func formatDoltDSN(user, network, address, dbName string, opts dsnOpts) string {
	if user == "" {
		user = "root"
	}
	qs := opts.queryString()
	dsn := fmt.Sprintf("%s@%s(%s)/%s", user, network, address, dbName)
	if qs == "" {
		return dsn
	}
	return dsn + "?" + qs
}

// buildDoltDSN produces a Go-MySQL-driver DSN that prefers the local
// Dolt unix domain socket when present, falling back to TCP loopback
// otherwise. The dbName, user, port, and query options are substituted
// into both forms.
//
// Rationale: short-lived gt-CLI subcommands over TCP loopback create a
// TIME_WAIT entry per close that lingers ~30s on macOS (2*MSL with
// MSL=15s). On busy rigs (background daemons, cron, periodic gt
// health/doctor/maintain invocations) the count climbs past
// port-monitor alert thresholds and risks port exhaustion. Unix-socket
// transport bypasses TIME_WAIT entirely.
//
// dc-fsue's metadata.json fix only covered the `bd` CLI side. wa-d6f
// tracks the remaining gt-CLI callsites that build their own DSNs
// inline (internal/cmd/health.go, maintain.go, dolt_flatten.go,
// dolt_rebase.go, install.go). This helper is the unified migration
// point so future callsites get socket-first transport for free.
//
// Conservative semantics: callers receive the TCP DSN whenever the
// default Dolt socket is not currently usable at the expected path
// (Windows, no Dolt running, custom socket path). No behavior change for
// setups without a local Dolt.
func buildDoltDSN(user string, port int, dbName string, opts dsnOpts) string {
	return buildDoltDSNVia(localDoltSocketPath, user, port, dbName, opts)
}

// buildDoltDSNVia is buildDoltDSN with the live socket for a port found by
// socketFor ("" when none).
func buildDoltDSNVia(socketFor func(port int) string, user string, port int, dbName string, opts dsnOpts) string {
	if sock := socketFor(port); sock != "" {
		return formatDoltDSN(user, "unix", sock, dbName, opts)
	}
	return formatDoltDSN(user, "tcp", fmt.Sprintf("127.0.0.1:%d", port), dbName, opts)
}

// buildDoltDSNFromConfig is a convenience wrapper that pulls user, port,
// and host from a *doltserver.Config (matches the maintain.go /
// dolt_flatten.go / dolt_rebase.go callsite pattern).
func buildDoltDSNFromConfig(c *doltserver.Config, dbName string, opts dsnOpts) string {
	return buildDoltDSNFromConfigVia(localDoltSocketPath, c, dbName, opts)
}

// buildDoltDSNFromConfigVia is buildDoltDSNFromConfig with the live socket for
// a port found by socketFor ("" when none).
func buildDoltDSNFromConfigVia(socketFor func(port int) string, c *doltserver.Config, dbName string, opts dsnOpts) string {
	if !c.IsRemote() {
		if sock := socketFor(c.Port); sock != "" {
			return formatDoltDSN(c.User, "unix", sock, dbName, opts)
		}
	}
	return formatDoltDSN(c.User, "tcp", c.HostPort(), dbName, opts)
}
