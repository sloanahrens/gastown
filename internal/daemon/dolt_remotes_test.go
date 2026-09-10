package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/testutil"
)

// testDoltRemotesDaemon returns a *Daemon that resolves the running Dolt
// server through the package's shared ephemeral container (started by
// TestMain via testutil.WithDolt) rather than the live town on :3307. Skips
// if no container is available (e.g. Docker missing).
func testDoltRemotesDaemon(t *testing.T) *Daemon {
	t.Helper()
	containerPort := testutil.DoltContainerPort()
	if containerPort == "" {
		t.Skip("no shared Dolt container available")
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

// createTestDB creates a fresh Dolt database on the shared server and
// returns its name, guaranteed not to collide with the "test"/"beads_t"/
// "beads_pt"/"doctest_" prefixes pushDatabase refuses to touch — several
// tests below push this database over a real (file://) remote and must not
// trip that refusal. "dolt_remotes_check_" is registered alongside those
// prefixes at the orphan-cleanup call sites that match on database-name
// prefix (jsonl_git_backup's discoverJsonlBackupDatabases, the reaper's
// testPollutionPrefixes, and gt dolt cleanup's filesystem fallback) so a
// leaked database here is still recognized as test cruft even though it
// isn't itself referenced by any rig's metadata.json.
func createTestDB(t *testing.T, d *Daemon) string {
	t.Helper()

	admin, err := d.openDoltDB("information_schema")
	if err != nil {
		t.Fatalf("connect to server: %v", err)
	}

	dbName := fmt.Sprintf("dolt_remotes_check_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", dbName)); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}

	// admin must stay open until this Cleanup runs at the end of the test —
	// not closed here via a plain defer, which would fire as soon as
	// createTestDB returns and leave the DROP below running on a closed
	// *sql.DB, silently skipped and leaking dolt_remotes-prefixed databases
	// on whatever server the test reached.
	t.Cleanup(func() {
		defer admin.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName)); err != nil {
			t.Logf("drop database %s: %v", dbName, err)
		}
	})

	return dbName
}

func TestDatabaseHasRemote_NoneConfigured(t *testing.T) {
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t, d)

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
	dbName := createTestDB(t, d)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	dbName := createTestDB(t, d)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

// TestOpenDoltDB_SurvivesQueryLongerThanOldReadTimeout locks in the DSN fix:
// openDoltDB's readTimeout must exceed doltPushTimeout, not sit below it.
// Before the fix, readTimeout=30s meant any query running longer than 30s —
// including a large, legitimate DOLT_PUSH within its 60s budget — died with a
// raw i/o timeout from the MySQL driver's socket read deadline, regardless of
// how much time was left on the request context. A query held open for 45s
// (comfortably inside doltPushTimeout, well past the old 30s ceiling) must
// still succeed.
func TestOpenDoltDB_SurvivesQueryLongerThanOldReadTimeout(t *testing.T) {
	d := testDoltRemotesDaemon(t)

	conn, err := d.openDoltDB("information_schema")
	if err != nil {
		t.Fatalf("connect to server: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer cancel()

	const sleepFor = 45 * time.Second
	start := time.Now()
	if _, err := conn.ExecContext(ctx, "SELECT SLEEP(?)", sleepFor.Seconds()); err != nil {
		t.Fatalf("SELECT SLEEP(%v) failed (readTimeout regressed below doltPushTimeout?): %v", sleepFor, err)
	}
	if elapsed := time.Since(start); elapsed < sleepFor {
		t.Fatalf("SELECT SLEEP(%v) returned early after %v", sleepFor, elapsed)
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
	dbName := createTestDB(t, d)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
