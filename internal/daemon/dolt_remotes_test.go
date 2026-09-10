package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// testDoltRemotesDaemon returns a *Daemon that resolves the running Dolt
// server through the package's shared ephemeral container (started by
// TestMain via testutil.WithDolt) rather than the live town on :3307. Skips
// if no container is available (e.g. Docker missing).
func testDoltRemotesDaemon(t *testing.T) *Daemon {
	t.Helper()
	if testutil.DoltContainerPort() == "" {
		t.Skip("no shared Dolt container available")
	}
	return &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0)}
}

// createTestDB creates a fresh Dolt database on the shared server and
// returns its name, guaranteed not to collide with the "test"/"beads_t"/
// "beads_pt"/"doctest_" prefixes pushDatabase refuses to touch.
func createTestDB(t *testing.T, d *Daemon) string {
	t.Helper()

	admin, err := d.openDoltDB("information_schema")
	if err != nil {
		t.Fatalf("connect to server: %v", err)
	}
	defer admin.Close()

	dbName := fmt.Sprintf("dolt_remotes_check_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", dbName)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		_, _ = admin.ExecContext(dropCtx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName))
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
