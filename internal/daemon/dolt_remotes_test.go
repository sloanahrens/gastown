package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/testutil"
)

// testDoltRemotesDaemon returns a *Daemon that resolves the running Dolt
// server through the package's shared ephemeral container (started by
// TestMain via testutil.WithDolt) rather than the live town on :3307. Without
// the GT_TEST_DOCKER=1 opt-in there is no container and the test skips; once
// opted in, a missing container fails it.
func testDoltRemotesDaemon(t *testing.T) *Daemon {
	t.Helper()
	containerPort := testutil.DoltContainerPort()
	if containerPort == "" {
		if testutil.DockerTestsEnabled() {
			t.Fatal("no shared Dolt container, though " + testutil.DockerTestsEnv + "=1 opted in")
		}
		t.Skip("no shared Dolt container: container-backed tests are opt-in (" + testutil.DockerTestsEnv + "=1)")
	}

	d := &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0)}

	// Refuse to run against anything but the package's ephemeral container.
	// d.doltServerPort() falls back to doltserver.DefaultPort (3307) — the
	// live production town — whenever GT_DOLT_PORT hasn't propagated to this
	// process. Silently proceeding in that case would let every test below
	// create and drop databases on production instead of the disposable
	// container.
	if port := d.doltServerPort(); strconv.Itoa(port) != containerPort {
		t.Fatalf("refusing to run: Dolt port resolved to %d (production default is %d), want ephemeral container port %s (GT_DOLT_PORT not propagated?)",
			port, doltserver.DefaultPort, containerPort)
	}

	return d
}

// testDoltSQLTimeout bounds the SQL these tests issue themselves against the
// package's container (each test's setup and verification statements). Dolt
// DDL and commits are fsync-bound and serialized on the server, and the
// container shares the Docker VM's disk with every other package's container
// during a -p=8 gate: a 15s bound failed at host load ~59 (gt-81fp6). It guards
// only a disposable local container, so it costs time only when that container
// is truly wedged.
const testDoltSQLTimeout = 2 * time.Minute

// createTestDB hands the test an empty database on the shared container and
// returns its name. The database was created with the container's pool before
// any test ran, and is never dropped: a CREATE or DROP DATABASE while other
// tests run breaks their store opens and migrations, and the pool's teardown
// guard fails the package on one (internal/testutil/doltpool.go). Its name
// carries the "dolt_remotes_check_" prefix, which pushDatabase does not refuse
// — several tests below push it over a real (file://) remote — and which the
// orphan cleanups treat as test cruft.
func createTestDB(t *testing.T) string {
	t.Helper()
	return testutil.TakePooledSQLDatabase(t)
}

func TestDatabaseHasRemote_NoneConfigured(t *testing.T) {
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t)

	if d.databaseHasRemote(dbName, "origin") {
		t.Fatalf("databaseHasRemote(%q) = true, want false (no remote configured)", dbName)
	}
	if d.databaseHasAnyRemote(dbName) {
		t.Fatalf("databaseHasAnyRemote(%q) = true, want false (no remote configured)", dbName)
	}
	if got := d.findDatabaseRemote(dbName); got != "" {
		t.Fatalf("findDatabaseRemote(%q) = %q, want empty", dbName, got)
	}
}

func TestDatabaseHasRemote_Configured(t *testing.T) {
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testDoltSQLTimeout)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "CALL DOLT_REMOTE('add', 'origin', 'file:///nonexistent/dolt-remote')"); err != nil {
		t.Fatalf("CALL DOLT_REMOTE add: %v", err)
	}

	if !d.databaseHasRemote(dbName, "origin") {
		t.Errorf("databaseHasRemote(%q, origin) = false, want true", dbName)
	}
	if d.databaseHasRemote(dbName, "upstream") {
		t.Errorf("databaseHasRemote(%q, upstream) = true, want false (only origin configured)", dbName)
	}
	if !d.databaseHasAnyRemote(dbName) {
		t.Errorf("databaseHasAnyRemote(%q) = false, want true", dbName)
	}
	if got := d.findDatabaseRemote(dbName); got != "origin" {
		t.Errorf("findDatabaseRemote(%q) = %q, want %q", dbName, got, "origin")
	}
}

func TestHasStagedChanges(t *testing.T) {
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testDoltSQLTimeout)
	defer cancel()

	staged, err := d.hasStagedChanges(ctx, conn)
	if err != nil {
		t.Fatalf("hasStagedChanges (empty db): %v", err)
	}
	if staged {
		t.Fatalf("hasStagedChanges (empty db) = true, want false")
	}

	if _, err := conn.ExecContext(ctx, "CREATE TABLE probe (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		t.Fatalf("CALL DOLT_ADD: %v", err)
	}

	staged, err = d.hasStagedChanges(ctx, conn)
	if err != nil {
		t.Fatalf("hasStagedChanges (staged): %v", err)
	}
	if !staged {
		t.Fatalf("hasStagedChanges (staged) = false, want true")
	}
}

// TestPushDatabase_RefusesTestPrefixes locks in the last line of defense
// against pushing pollution to a real remote: any database name matching a
// known test prefix is refused before any SQL runs.
func TestPushDatabase_RefusesTestPrefixes(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0)}

	for _, name := range []string{"test_foo", "beads_t1234", "beads_pt5678", "doctest_abc"} {
		err := d.pushDatabase(name, "origin", "main")
		if err == nil {
			t.Errorf("pushDatabase(%q) = nil error, want refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), "REFUSED") {
			t.Errorf("pushDatabase(%q) error = %q, want it to mention REFUSED", name, err)
		}
	}
}

// TestOpenDoltDB_ReadTimeoutExceedsPushTimeout locks in the DSN fix:
// openDoltDB's socket readTimeout (and writeTimeout) must exceed
// doltPushTimeout, not sit below it. Before the fix, readTimeout=30s meant
// any query running longer than 30s, including a large, legitimate DOLT_PUSH
// within its 60s budget, died with a raw i/o timeout from the MySQL driver's
// socket read deadline, regardless of how much time was left on the request
// context. The request context must be what bounds a push, so the driver's
// deadline has to sit past it.
//
// This asserts on the exact DSN openDoltDB connects with. Whether the MySQL
// driver enforces a DSN readTimeout is the driver's contract, not gastown's,
// so it is not re-tested here.
func TestOpenDoltDB_ReadTimeoutExceedsPushTimeout(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0)}

	cfg, err := mysql.ParseDSN(d.doltRemotesDSN("information_schema"))
	if err != nil {
		t.Fatalf("parse openDoltDB DSN: %v", err)
	}
	if cfg.ReadTimeout <= doltPushTimeout {
		t.Errorf("openDoltDB readTimeout = %v, want > doltPushTimeout (%v)", cfg.ReadTimeout, doltPushTimeout)
	}
	if cfg.WriteTimeout <= doltPushTimeout {
		t.Errorf("openDoltDB writeTimeout = %v, want > doltPushTimeout (%v)", cfg.WriteTimeout, doltPushTimeout)
	}
}

// TestDoltRemotesDSN_LongQueryCompletesWithinContextBudget runs a query that
// holds the connection open, over a dolt_remotes DSN, and requires it to run
// to completion. The DSN read timeout sits above the context budget, as
// doltRemotesReadTimeout sits above doltPushTimeout, and both are generous
// (testDoltSQLTimeout), so a stalled container slows the test rather than
// failing it. It replaces a 45s SELECT SLEEP with a 1.5s one.
func TestDoltRemotesDSN_LongQueryCompletesWithinContextBudget(t *testing.T) {
	d := testDoltRemotesDaemon(t)

	const sleepFor = 1500 * time.Millisecond
	dsn := doltRemotesDSNWithTimeout(d.doltServerHost(), d.doltServerPort(), "information_schema", testDoltSQLTimeout+30*time.Second)
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open(dolt_remotes DSN): %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testDoltSQLTimeout)
	defer cancel()

	start := time.Now()
	var slept int64
	if err := conn.QueryRowContext(ctx, "SELECT SLEEP(?)", sleepFor.Seconds()).Scan(&slept); err != nil {
		t.Fatalf("SELECT SLEEP(%v) with read timeout past the context budget: %v", sleepFor, err)
	}
	elapsed := time.Since(start)
	if slept != 0 {
		t.Fatalf("SELECT SLEEP(%v) = %d, want 0 (sleep ran to completion, not interrupted)", sleepFor, slept)
	}
	if elapsed < sleepFor {
		t.Fatalf("SELECT SLEEP(%v) returned after %v, want at least %v", sleepFor, elapsed, sleepFor)
	}
}

// TestPushDatabase_UsesLiveServerConnection exercises the full add/commit/push
// sequence against the shared server over the same TCP connection the running
// dolt-sql-server owns — never a competing "dolt" CLI process against the
// on-disk data directory (the concurrent-writer hazard this rewrite removes).
// The remote is a file:// path inside the server's own filesystem, so the
// push genuinely succeeds; the test verifies the remote-tracking ref actually
// advanced to local HEAD, proving DOLT_ADD, DOLT_COMMIT and DOLT_PUSH all ran
// correctly over the live connection rather than against a stale/independent
// checkout of the data directory.
func TestPushDatabase_UsesLiveServerConnection(t *testing.T) {
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testDoltSQLTimeout)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE probe (id INT PRIMARY KEY)"); err != nil {
		conn.Close()
		t.Fatalf("create table: %v", err)
	}
	remotePath := fmt.Sprintf("file:///tmp/%s-remote", dbName)
	if _, err := conn.ExecContext(ctx, "CALL DOLT_REMOTE('add', 'origin', ?)", remotePath); err != nil {
		conn.Close()
		t.Fatalf("CALL DOLT_REMOTE add: %v", err)
	}
	conn.Close()

	if err := d.pushDatabase(dbName, "origin", "main"); err != nil {
		t.Fatalf("pushDatabase(%q) = %v, want success", dbName, err)
	}

	verify, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("reconnect to %s: %v", dbName, err)
	}
	defer verify.Close()

	// pushDatabase must have staged and committed the new table before pushing.
	staged, err := d.hasStagedChanges(ctx, verify)
	if err != nil {
		t.Fatalf("hasStagedChanges after pushDatabase: %v", err)
	}
	if staged {
		t.Errorf("hasStagedChanges after pushDatabase = true, want false (pushDatabase should have committed)")
	}

	// The remote-tracking ref must have advanced to local HEAD, proving the
	// push actually landed rather than silently no-op'ing.
	var localHead string
	if err := verify.QueryRowContext(ctx, "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1").Scan(&localHead); err != nil {
		t.Fatalf("query local HEAD: %v", err)
	}
	var remoteHead string
	if err := verify.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ?", "remotes/origin/main").Scan(&remoteHead); err != nil {
		t.Fatalf("query remote-tracking ref: %v", err)
	}
	if remoteHead != localHead {
		t.Errorf("remote-tracking origin/main = %s, want it to match local HEAD %s", remoteHead, localHead)
	}
}

// runBounded is what pushDoltRemotesBounded relies on to keep an unbounded
// callee from making daemon shutdown (and so a restart, see waitForRestart
// in internal/cmd) open-ended (gt-oqbw). This exercises both
// branches directly, decoupled from a real Dolt server: the alarming one
// (fn outlives the budget: onTimeout must fire and runBounded must return
// without waiting for fn) and the ordinary one (fn finishes first: onTimeout
// must not fire).
func TestRunBounded_FiresOnTimeoutAndDoesNotWaitForTheAbandonedCall(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	fnDone := make(chan struct{})
	var timedOut bool

	before := time.Now()
	runBounded(10*time.Millisecond, func() {
		<-release // held open well past the budget
		close(fnDone)
	}, func() { timedOut = true }) // onTimeout runs synchronously in this goroutine, before runBounded returns
	elapsed := time.Since(before)

	if !timedOut {
		t.Error("onTimeout was not called although fn outlived the budget")
	}
	// 2s, not a tighter bound near the 10ms budget: this town's own gate
	// (container builds, Dolt operations, other suites sharing the host) can
	// starve the goroutine scheduler badly enough to blow a tight wall-clock
	// assertion without runBounded itself being late — the property under
	// test is "returns without waiting for fn", not a tight latency bound.
	if elapsed > 2*time.Second {
		t.Errorf("runBounded took %v to return, want it bounded near the 10ms budget regardless of fn", elapsed)
	}
	select {
	case <-fnDone:
		t.Error("fn had already finished when runBounded returned; the test does not exercise abandonment")
	default:
		// fn is still running in the background, as intended.
	}
	close(release)
	<-fnDone
}

func TestRunBounded_NoTimeoutWhenFnFinishesFirst(t *testing.T) {
	t.Parallel()
	var timedOut bool
	runBounded(time.Second, func() {}, func() { timedOut = true })
	if timedOut {
		t.Error("onTimeout was called although fn finished well within the budget")
	}
}
