package doltserver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestFetchAndVerify exercises the shared remote-divergence pre-flight against
// a real Dolt server. The pre-flight is the only thing standing between a
// flatten and the silent destruction of whatever only the remote had, so it is
// tested against real remotes rather than a fake: the query shapes
// (dolt_remotes, dolt_remote_branches, dolt_log) are the part most likely to
// rot, and a fake would encode the same assumptions as the implementation.
func TestFetchAndVerify(t *testing.T) {
	admin := doltTestAdmin(t)

	t.Run("no remote has nothing to verify", func(t *testing.T) {
		conn, name := createDivergenceTestDB(t, admin)
		seedHistory(t, conn, name)

		got, err := fetchAndVerify(t, conn, name)
		if err != nil {
			t.Fatalf("FetchAndVerify on a remote-less database: %v", err)
		}
		if got.Remote != "" {
			t.Errorf("Remote = %q, want empty for a database with no remote", got.Remote)
		}
		if got.Diverged {
			t.Error("a database with no remote reported divergence — nothing can be lost to a flatten")
		}
	})

	t.Run("pushed and unchanged is not diverged", func(t *testing.T) {
		conn, name := createDivergenceTestDB(t, admin)
		seedHistory(t, conn, name)
		addRemoteAndPush(t, conn, name)

		got, err := fetchAndVerify(t, conn, name)
		if err != nil {
			t.Fatalf("FetchAndVerify after a push: %v", err)
		}
		if got.Remote != "origin" {
			t.Errorf("Remote = %q, want origin", got.Remote)
		}
		if got.RemoteHead == "" {
			t.Error("RemoteHead is empty after a push — the remote main branch was not read")
		}
		if got.Diverged {
			t.Errorf("reported divergence with remote head %s for a database that just pushed to it", got.RemoteHead)
		}
	})

	// The load-bearing case. A consumer database with an unrelated history
	// learns of the remote only through the fetch, so an implementation that
	// skipped DOLT_FETCH would find no remote-tracking ref at all and clear
	// this database — the exact fail-open the guard must not have.
	t.Run("remote holding commits local lacks is diverged", func(t *testing.T) {
		producer, producerName := createDivergenceTestDB(t, admin)
		seedHistory(t, producer, producerName)
		remote := remoteURL(producerName)
		addRemoteAndPush(t, producer, producerName)

		consumer, consumerName := createDivergenceTestDB(t, admin)
		seedHistory(t, consumer, consumerName)
		execSQL(t, consumer, "CALL DOLT_REMOTE('add','origin',?)", remote)

		// Confirm the premise: without the fetch the consumer has no idea what
		// the remote holds.
		var tracking int
		if err := consumer.QueryRow(
			"SELECT COUNT(*) FROM dolt_remote_branches WHERE name = 'remotes/origin/main'",
		).Scan(&tracking); err != nil {
			t.Fatalf("count remote-tracking refs before fetch: %v", err)
		}
		if tracking != 0 {
			t.Fatalf("consumer already tracks %d remote ref(s) before any fetch — this case can no longer prove the fetch ran", tracking)
		}

		got, err := fetchAndVerify(t, consumer, consumerName)
		if err != nil {
			t.Fatalf("FetchAndVerify on a diverged consumer: %v", err)
		}
		if !got.Diverged {
			t.Fatalf("consumer reported no divergence against a remote whose history it does not share (remote=%q head=%q) — the fetch did not run, or the tracking ref was read from the wrong name",
				got.Remote, got.RemoteHead)
		}
		if got.Remote != "origin" {
			t.Errorf("Remote = %q, want origin", got.Remote)
		}
		if got.RemoteHead == "" {
			t.Error("Diverged is set but RemoteHead is empty — the verdict is not attributable")
		}
	})

	// The real-world shape of the hazard: a flatten rewrites local history,
	// the remote still points at the pre-flatten graph, and the next
	// force-push would drop those commits. The pre-flight must fire here.
	t.Run("a flatten leaves the database diverged from its unpushed remote", func(t *testing.T) {
		conn, name := createDivergenceTestDB(t, admin)
		seedHistory(t, conn, name)
		addRemoteAndPush(t, conn, name)

		before, err := fetchAndVerify(t, conn, name)
		if err != nil {
			t.Fatalf("FetchAndVerify before flatten: %v", err)
		}
		if before.Diverged {
			t.Fatalf("premise broken: %s is already diverged before the flatten (head=%s)", name, before.RemoteHead)
		}

		flattenHistory(t, conn, name)

		after, err := fetchAndVerify(t, conn, name)
		if err != nil {
			t.Fatalf("FetchAndVerify after flatten: %v", err)
		}
		if !after.Diverged {
			t.Fatalf("flatten left the remote holding commits local no longer has, but the pre-flight cleared it (head=%s)", after.RemoteHead)
		}
	})

	// The fail-open this guard must not have: a database can have more than
	// one remote, and a check that only looked at the alphabetically-first
	// one would clear the whole database on that remote's say-so alone,
	// leaving a second, genuinely diverged remote unexamined.
	t.Run("divergence on a remote that does not sort first is still caught", func(t *testing.T) {
		consumer, consumerName := createDivergenceTestDB(t, admin)
		seedHistory(t, consumer, consumerName)

		// "aardvark" sorts before "zzremote". Push the consumer's own current
		// history there, so this remote is provably not diverged.
		execSQL(t, consumer, "CALL DOLT_REMOTE('add','aardvark',?)", remoteURL(consumerName+"-aardvark"))
		execSQL(t, consumer, "CALL DOLT_PUSH('aardvark','main')")

		// "zzremote" points at a producer database with unrelated history the
		// consumer does not have.
		producer, producerName := createDivergenceTestDB(t, admin)
		seedHistory(t, producer, producerName)
		addRemoteAndPush(t, producer, producerName)
		execSQL(t, consumer, "CALL DOLT_REMOTE('add','zzremote',?)", remoteURL(producerName))

		got, err := fetchAndVerify(t, consumer, consumerName)
		if err != nil {
			t.Fatalf("FetchAndVerify with two remotes: %v", err)
		}
		if !got.Diverged {
			t.Fatalf("consumer has a second, diverged remote (zzremote) but the guard cleared it — only the first remote (aardvark) was examined")
		}
		if got.Remote != "zzremote" {
			t.Errorf("Remote = %q, want zzremote (the remote the divergence was found on)", got.Remote)
		}
	})
}

// TestFetchAndVerifyRejectsInvalidDatabaseName pins the identifier check: the
// database name is interpolated into a query by callers, so a name that is not
// a plain identifier must be refused before anything reaches the server.
func TestFetchAndVerifyRejectsInvalidDatabaseName(t *testing.T) {
	admin := doltTestAdmin(t)
	conn, _ := createDivergenceTestDB(t, admin)

	for _, name := range []string{"", "bad name", "db;DROP DATABASE gt", "db`x"} {
		if _, err := fetchAndVerify(t, conn, name); err == nil {
			t.Errorf("FetchAndVerify(%q) = nil error, want a refusal", name)
		} else if !strings.Contains(err.Error(), "invalid database name") {
			t.Errorf("FetchAndVerify(%q) error = %v, want it to name the invalid identifier", name, err)
		}
	}
}

// --- helpers ---------------------------------------------------------------

// doltTestAdmin returns a server-level connection to the shared ephemeral Dolt
// container, skipping when container tests are not opted in.
func doltTestAdmin(t *testing.T) *sql.DB {
	t.Helper()
	testutil.RequireDoltContainer(t)

	dsn := "root:@tcp(127.0.0.1:" + testutil.DoltContainerPort() + ")/"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("ping Dolt container: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// createDivergenceTestDB creates a fresh database on the container and returns
// a connection bound to it. The "dolt_remotes_check_" prefix is registered with
// the orphan-cleanup call sites that match database names, so a leaked database
// here is still recognized as test cruft.
func createDivergenceTestDB(t *testing.T, admin *sql.DB) (*sql.DB, string) {
	t.Helper()

	name := fmt.Sprintf("dolt_remotes_check_div_%d", time.Now().UnixNano())
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE `%s`", name)); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", name)); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
		_, _ = admin.Exec("CALL dolt_purge_dropped_databases()")
	})

	dsn := "root:@tcp(127.0.0.1:" + testutil.DoltContainerPort() + ")/" + name + "?parseTime=true"
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open connection to %s: %v", name, err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		t.Fatalf("ping %s: %v", name, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, name
}

// remoteURL returns a file:// remote path for a database. The path is resolved
// inside the container, not on the host.
func remoteURL(dbName string) string {
	return "file:///tmp/" + dbName + "-remote"
}

// seedHistory gives a fresh database at least two commits. Two matters: a
// flatten reset to the root keeps the root commit, so divergence against the
// remote head is only observable when the remote head is not the root.
func seedHistory(t *testing.T, conn *sql.DB, dbName string) {
	t.Helper()

	execSQL(t, conn, "CREATE TABLE probe (id INT PRIMARY KEY)")
	commit(t, conn, "probe table")
	execSQL(t, conn, fmt.Sprintf("INSERT INTO `%s`.probe VALUES (1)", dbName))
	commit(t, conn, "probe row")
}

func addRemoteAndPush(t *testing.T, conn *sql.DB, dbName string) {
	t.Helper()
	execSQL(t, conn, "CALL DOLT_REMOTE('add','origin',?)", remoteURL(dbName))
	execSQL(t, conn, "CALL DOLT_PUSH('origin','main')")
}

// flattenHistory squashes the database's history to its root commit, the same
// operation `gt maintain` performs.
func flattenHistory(t *testing.T, conn *sql.DB, dbName string) {
	t.Helper()

	var root string
	if err := conn.QueryRow(fmt.Sprintf(
		"SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", dbName,
	)).Scan(&root); err != nil {
		t.Fatalf("find root commit of %s: %v", dbName, err)
	}
	execSQL(t, conn, fmt.Sprintf("CALL DOLT_RESET('--soft','%s')", root))
	commitAll(t, conn, "flatten history")
}

func fetchAndVerify(t *testing.T, conn *sql.DB, dbName string) (doltserver.RemoteDivergence, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return doltserver.FetchAndVerify(ctx, conn, dbName)
}

func commit(t *testing.T, conn *sql.DB, message string) {
	t.Helper()
	execSQL(t, conn, "CALL DOLT_ADD('-A')")
	execSQL(t, conn, fmt.Sprintf(
		"CALL DOLT_COMMIT('-m', '%s', '--author', 'gt-test <gt-test@localhost>')", message))
}

func commitAll(t *testing.T, conn *sql.DB, message string) {
	t.Helper()
	execSQL(t, conn, fmt.Sprintf(
		"CALL DOLT_COMMIT('-Am', '%s', '--author', 'gt-test <gt-test@localhost>')", message))
}

func execSQL(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
