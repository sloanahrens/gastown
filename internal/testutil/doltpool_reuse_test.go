//go:build !windows

package testutil

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

// TestDoltPoolResetsAReturnedDatabaseOnARealServer is the reuse proof against
// the real image: a one-database pool lends its store to a test that dirties
// it every way a test can — issues, a wisp in a dolt_ignore table, config, a
// branch, a tag, a remote, a table of its own, uncommitted rows — and the next
// lessee gets the same database back exactly as the migration left it. A bd
// init lease then gets it back empty, and a SQL database is reset the same
// way. The server's catalog is the same at the end as at the start. It runs on
// its own container, so its pool cannot reach any other test.
func TestDoltPoolResetsAReturnedDatabaseOnARealServer(t *testing.T) {
	port := StartIsolatedDoltContainer(t)
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_TEST_MODE", "1")
	t.Setenv("BEADS_DOLT_PORT", port)
	t.Setenv("BEADS_DOLT_SERVER_PORT", port)
	t.Setenv("BEADS_TEST_SERVER", "1")

	p, err := newDoltDBPool(portNum, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	if err := p.create(); err != nil {
		t.Fatal(err)
	}
	e := p.stores[0]
	db, err := sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/"+e.name+"?timeout=30s")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// No idle connection: a session left open on a returned database fails
	// its release (see the leaked-session case below).
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	ctx := context.Background()

	var migratedHead string
	t.Run("first lessee dirties it", func(t *testing.T) {
		store := openPooledStore(t, ctx, p)
		migratedHead = e.head
		if !e.migrated || migratedHead == e.initCommit {
			t.Fatalf("after the first open the entry is migrated=%v at %s; want the migration's commit recorded", e.migrated, migratedHead)
		}
		if err := store.SetConfig(ctx, "issue_prefix", "dirty"); err != nil {
			t.Fatal(err)
		}
		for _, issue := range []*beadsdk.Issue{
			{Title: "committed issue", Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask},
			{Title: "wisp", Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask, Ephemeral: true},
		} {
			if err := store.CreateIssue(ctx, issue, "dirtier"); err != nil {
				t.Fatalf("create %q: %v", issue.Title, err)
			}
		}
		for _, q := range []string{
			"CALL DOLT_BRANCH('side')",
			"CALL DOLT_TAG('v1', 'HEAD')",
			"CALL DOLT_REMOTE('add', 'origin', 'file:///nonexistent/dolt-remote')",
			"CREATE TABLE stray (id INT PRIMARY KEY)",
			"INSERT INTO stray VALUES (1)",
			"INSERT INTO config (`key`, value) VALUES ('uncommitted', 'x')",
		} {
			mustExec(t, db, q)
		}
		requireCount(t, db, "SELECT COUNT(*) FROM issues", 1)
		requireCount(t, db, "SELECT COUNT(*) FROM wisps", 1)
	})
	if e.leased {
		t.Fatal("the first lessee's database was not returned when it ended")
	}

	t.Run("next lessee sees it pristine", func(t *testing.T) {
		store := openPooledStore(t, ctx, p)
		if e.head != migratedHead {
			t.Errorf("entry head %s, want the migration's %s", e.head, migratedHead)
		}
		requireOnly(t, db, "SELECT hash FROM dolt_branches", migratedHead)
		requireOnly(t, db, "SELECT name FROM dolt_branches", "main")
		requireOnly(t, db, "SELECT tag_name FROM dolt_tags")
		requireOnly(t, db, "SELECT name FROM dolt_remotes")
		requireOnly(t, db, "SELECT table_name FROM dolt_status")
		requireCount(t, db, "SELECT COUNT(*) FROM issues", 0)
		requireCount(t, db, "SELECT COUNT(*) FROM wisps", 0)
		requireCount(t, db, "SELECT COUNT(*) FROM config WHERE `key` IN ('issue_prefix', 'uncommitted')", 0)
		requireCount(t, db, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'stray'", 0)
		// The store works on the returned database.
		if err := store.SetConfig(ctx, "issue_prefix", "clean"); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateIssue(ctx, &beadsdk.Issue{Title: "after reuse", Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask}, "t"); err != nil {
			t.Fatalf("create on the reused database: %v", err)
		}
	})
	if p.leases != 2 || p.reuses != 1 {
		t.Errorf("leases %d reuses %d, want 2 and 1", p.leases, p.reuses)
	}

	// A bd init needs an empty database: the migrated one goes back to the
	// commit CREATE DATABASE made.
	init, err := p.acquire(leaseInit, "bd init", "")
	if err != nil {
		t.Fatal(err)
	}
	if init != e {
		t.Fatalf("bd init lease got %s, want the only store database %s", init.name, e.name)
	}
	requireOnly(t, db, "SELECT hash FROM dolt_branches", e.initCommit)
	requireOnly(t, db, "SHOW TABLES")

	sqlEntry, err := p.acquire(leaseSQL, "sql", "")
	if err != nil {
		t.Fatal(err)
	}
	sdb, err := sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/"+sqlEntry.name+"?timeout=30s")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)
	sdb.SetMaxIdleConns(0)
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY)",
		"INSERT INTO t VALUES (1)",
		"CALL DOLT_COMMIT('-Am', 'test commit')",
		"CALL DOLT_REMOTE('add', 'origin', 'file:///nonexistent/dolt-remote')",
		"CALL DOLT_BRANCH('side')",
		"INSERT INTO t VALUES (2)",
		"CALL DOLT_STASH('push', 'leftover')",
	} {
		mustExec(t, sdb, q)
	}
	if err := p.release(sqlEntry); err != nil {
		t.Fatalf("release the SQL database: %v", err)
	}
	requireOnly(t, sdb, "SELECT CONCAT(name, '@', hash) FROM dolt_branches", "main@"+sqlEntry.initCommit)
	requireOnly(t, sdb, "SHOW TABLES")
	requireOnly(t, sdb, "SELECT name FROM dolt_remotes")
	requireOnly(t, sdb, "SELECT name FROM dolt_stashes")

	// A reset point holding an AUTO_INCREMENT table: a hard reset brings the
	// rows back but not the counter, which Dolt then stops reporting, so the
	// release must refuse the database rather than lend it with ids that
	// differ from a fresh one's.
	t.Run("moved AUTO_INCREMENT counter", func(t *testing.T) {
		ai, err := p.acquire(leaseSQL, "TestCounter", "")
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, sdb, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
		mustExec(t, sdb, "CALL DOLT_COMMIT('-Am', 'reset point with a counter')")
		if err := p.recordHead(ai); err != nil {
			t.Fatal(err)
		}
		mustExec(t, sdb, "INSERT INTO ai (v) VALUES (1), (2), (3)")
		err = p.release(ai)
		if err == nil || !strings.Contains(err.Error(), "AUTO_INCREMENT") || !strings.Contains(err.Error(), "TestCounter") {
			t.Fatalf("release = %v, want a refusal naming the AUTO_INCREMENT column and the lessee", err)
		}
		// Put the one SQL database back for the next case.
		p.mu.Lock()
		ai.broken, ai.leased, ai.head = nil, false, ai.initCommit
		p.mu.Unlock()
		if err := p.release(mustAcquire(t, p, leaseSQL, "cleanup")); err != nil {
			t.Fatal(err)
		}
	})

	// A lessee that leaves a session open with a transaction in flight: if the
	// release reset under it, the transaction could commit into the database
	// the next lessee gets. The release must end that session and refuse the
	// database, naming the lessee.
	t.Run("session left open across the release", func(t *testing.T) {
		p.sessionWait = time.Second
		leaky, err := p.acquire(leaseSQL, "TestLeaky", "")
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, sdb, "CREATE TABLE late (id INT PRIMARY KEY)")
		mustExec(t, sdb, "CALL DOLT_COMMIT('-Am', 'late table')")
		ldb, err := sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/"+leaky.name+"?timeout=30s")
		if err != nil {
			t.Fatal(err)
		}
		defer ldb.Close()
		tx, err := ldb.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO late VALUES (1)"); err != nil {
			t.Fatal(err)
		}
		err = p.release(leaky)
		if err == nil || !strings.Contains(err.Error(), "TestLeaky") || !strings.Contains(err.Error(), "open") {
			t.Fatalf("release with a session still open = %v, want a refusal naming the lessee", err)
		}
		commitErr := tx.Commit()
		if leaky.broken == nil {
			t.Error("the database was put back in the pool after its lessee left a session open")
		}
		if commitErr == nil {
			var n int
			_ = sdb.QueryRow("SELECT COUNT(*) FROM late").Scan(&n)
			t.Fatalf("the leaked transaction committed after the release (rows in late: %d); the session must be ended first", n)
		}
	})

	if err := verifyDoltCatalogAt(portNum, p.names); err != nil {
		t.Fatalf("leasing and resetting changed the catalog: %v", err)
	}
}

func requireCount(t *testing.T, db *sql.DB, q string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if n != want {
		t.Errorf("%s = %d, want %d", q, n, want)
	}
}

// requireOnly fails unless q's first column is exactly want, in any order.
func requireOnly(t *testing.T, db *sql.DB, q string, want ...string) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := queryStrings(context.Background(), conn, q)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", q, got, want)
	}
}

func mustAcquire(t *testing.T, p *doltDBPool, kind doltLeaseKind, owner string) *doltPoolEntry {
	t.Helper()
	e, err := p.acquire(kind, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}
