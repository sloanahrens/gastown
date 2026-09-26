package doltserver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// lostContainerMarkers are the error fragments that mean the shared test Dolt
// container stopped answering, rather than Dolt answering the query (gt-qkuj).
//
// This package's container-backed tests reach that container with a raw
// database/sql pool instead of through bd, so they cannot ride on
// internal/beads' bdConnectionFailureMarkers or on
// testutil.SkipOrFailContainerInit, which classifies bd's stderr. The
// vocabulary is deliberately the same one, though: an "invalid connection" from
// a test Dolt container means the same thing whichever client saw it.
//
// The signatures were read off the gate's own evidence (gt-qkuj, two
// consecutive `gt done` gate runs on 2026-09-21) and off a reproduction of it:
//
//	[mysql] 2026/09/21 12:56:55 packets.go:58 unexpected EOF
//	[mysql] 2026/09/21 12:56:55 packets.go:58 unexpected EOF
//	divergence_test.go:108: create database dolt_remotes_check_div_...: invalid connection
//	divergence_test.go:136: ping Dolt container: invalid connection
//
// A saturating host starves the Docker VM under the suite, and a starved Dolt
// server is indistinguishable from a dead one at this layer: the connection
// dies before the query is answered, so the failure lands in the query rather
// than in the response to it.
var lostContainerMarkers = []string{
	"invalid connection",       // go-sql-driver ErrInvalidConn: the connection died before use
	"unexpected EOF",           // the server closed the socket mid-packet
	"i/o timeout",              // read/write deadline on the Dolt socket
	"connection reset by peer", // the server dropped the connection
	"broken pipe",              // write to a connection the server already closed
	"connection refused",       // the container is gone: nothing is accepting
}

// isLostContainerErr reports whether err is a lost connection to the shared test
// Dolt container rather than an answer from it.
//
// Scoping is the point. A predicate that returned true for anything would turn
// every regression this suite exists to catch into a skip, which is the guard
// bypass gt-cbtl had to undo in internal/cmd. So the sentinel and the markers
// above are the whole test: a syntax error, a duplicate key, a divergence
// verdict that is wrong, or a bug that makes FetchAndVerify return any other
// error all keep failing the test.
func isLostContainerErr(err error) bool {
	if err == nil {
		return false
	}
	// The suite's own budgets are not a lost container. context.DeadlineExceeded
	// is FetchAndVerify's 90s fetch budget or the caller's; treating it as
	// environmental would make a genuinely hung container — the failure mode
	// the budget exists to bound — look the same as a starved one.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// The sentinel first: go-sql-driver returns ErrInvalidConn itself for a
	// connection it has already marked dead, and it is exported, so the common
	// case does not depend on a string.
	if errors.Is(err, mysql.ErrInvalidConn) {
		return true
	}
	// Folded case on both sides so the markers can be written the way the
	// errors actually read ("unexpected EOF") without the comparison depending
	// on it.
	msg := strings.ToLower(err.Error())
	for _, marker := range lostContainerMarkers {
		if strings.Contains(msg, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// skipOrFailContainerLost skips t when the shared test Dolt container stopped
// answering, and fails t for every other error (gt-qkuj).
//
// The skip is the same trade two sibling suites already make for a gone
// container (internal/cmd's patrol and rig_park suites, via
// testutil.SkipOrFailContainerInit, gt-cbtl): a container that vanished under a
// starved Docker VM is an absence of evidence, not evidence about the code
// under test. Which is why the classifier above is narrow — the moment it
// widens, the suite goes green under load while hiding the regressions it was
// written for.
//
// Called from a subtest this skips that subtest; the container stays gone, so
// the sibling subtests and every later test in the package skip too, instead of
// reddening the package on a container nobody can reach.
func skipOrFailContainerLost(t *testing.T, err error, what string) {
	t.Helper()
	if isLostContainerErr(err) {
		t.Skipf("test Dolt container gone, skipping: %s: %v", what, err)
	}
	t.Fatalf("%s: %v", what, err)
}

// TestIsLostContainerErr pins the classifier's two edges (gt-qkuj): it must
// recognise the lost-connection shapes the gate actually produced, and it must
// refuse everything else, because every false positive is a regression this
// suite stops catching.
func TestIsLostContainerErr(t *testing.T) {
	lost := []error{
		mysql.ErrInvalidConn,
		fmt.Errorf("create database dolt_remotes_check_div_17: %w", mysql.ErrInvalidConn),
		errors.New("[mysql] packets.go:58 unexpected EOF"),
		errors.New("read tcp 127.0.0.1:65473->127.0.0.1:55107: i/o timeout"),
		errors.New("read tcp 127.0.0.1:65473->127.0.0.1:55107: connection reset by peer"),
		errors.New("write tcp 127.0.0.1:65473->127.0.0.1:55107: broken pipe"),
		errors.New("dial tcp 127.0.0.1:55107: connect: connection refused"),
	}
	for _, err := range lost {
		if !isLostContainerErr(err) {
			t.Errorf("isLostContainerErr(%v) = false, want true (a lost container connection)", err)
		}
	}

	kept := []error{
		nil,
		// A query that spent the caller's own deadline is what the 90s budget
		// bounds, so it must keep failing — otherwise a genuinely hung
		// container reads the same as a starved one. This message carries a
		// connection marker alongside the deadline on purpose: it pins that the
		// deadline is read before the marker is.
		fmt.Errorf("DOLT_FETCH origin: read tcp 127.0.0.1:65473->127.0.0.1:55107: i/o timeout: %w", context.DeadlineExceeded),
		fmt.Errorf("DOLT_FETCH origin: %w", context.Canceled),
		// Dolt answering with a problem of its own is a regression, not a
		// lost container.
		errors.New("list remotes: Error 1146: table 'dolt_remotes' doesn't exist"),
		errors.New("DOLT_FETCH origin: remote not found"),
		errors.New("read remotes/origin/main: sql: no rows in result set"),
		errors.New("invalid database name \"bad name\": must match [a-zA-Z0-9_.-]+"),
		errors.New("DOLT_RESET('--soft','abc'): no changes to commit"),
	}
	for _, err := range kept {
		if isLostContainerErr(err) {
			t.Errorf("isLostContainerErr(%v) = true, want false — a skip here hides a regression", err)
		}
	}
}
