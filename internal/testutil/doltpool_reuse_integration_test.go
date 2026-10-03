//go:build integration && !windows

package testutil

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIntegrationDoltPoolResetsAReturnedDatabaseOnARealServer proves the
// pool's lease reset against the real Dolt image, the coverage gt-7iwy0.3.4
// removed with the in-process beads library its old integration test used
// (gt-0m13h). Every case is raw SQL over a lease, because the reset
// (resetDoltDatabase) is the same whichever kind of lease held the database.
//
// It builds a private pool on the scratch container (LeaseScratchDoltContainer)
// rather than leasing from the shared one (currentDoltPool): two of its three
// cases end in a refusal, which leaves that entry broken for the rest of the
// run, and the shared pool's databases must stay leasable by every other test.
func TestIntegrationDoltPoolResetsAReturnedDatabaseOnARealServer(t *testing.T) {
	port := LeaseScratchDoltContainer(t)
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("scratch Dolt port %q: %v", port, err)
	}

	// Three SQL databases: the reuse case returns one clean, and the two
	// refusals each break one, so no case depends on another's outcome.
	p, err := newDoltDBPool(portNum, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	if err := p.create(); err != nil {
		t.Fatal(err)
	}

	t.Run("a returned database is reset and is reusable", func(t *testing.T) {
		e, err := p.acquire(leaseSQL, "TestDirtier", "")
		if err != nil {
			t.Fatal(err)
		}
		db := openPoolSQL(t, port, e.name)

		// Dirty it every way a test can, and commit a point the pool never
		// recorded: the reset must return it to the commit the lease was
		// handed out at (e.initCommit), not the test's own.
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		mustExec(t, db, "INSERT INTO t VALUES (1, 10)")
		mustExec(t, db, "CALL DOLT_COMMIT('-Am', 'a commit the pool never recorded')")
		for _, q := range []string{
			"CALL DOLT_BRANCH('side')",
			"CALL DOLT_TAG('v1', 'HEAD')",
			"CALL DOLT_REMOTE('add', 'origin', 'file:///nonexistent/dolt-remote')",
			"INSERT INTO t VALUES (2, 20)",
		} {
			mustExec(t, db, q)
		}
		requireCount(t, db, "SELECT COUNT(*) FROM t", 2)
		for _, q := range []string{
			"CALL DOLT_STASH('push', 'leftover')",
			"CREATE TABLE stray (id INT PRIMARY KEY)",
			"INSERT INTO stray VALUES (1)",
		} {
			mustExec(t, db, q)
		}

		// No session of this test's may outlive the release, or the release
		// refuses the database instead of resetting it.
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := p.release(e); err != nil {
			t.Fatalf("release a dirty database: %v", err)
		}
		db = openPoolSQL(t, port, e.name)
		defer db.Close()

		requireOnly(t, db, "SELECT CONCAT(name, '@', hash) FROM dolt_branches", "main@"+e.initCommit)
		requireOnly(t, db, "SHOW TABLES")
		requireOnly(t, db, "SELECT tag_name FROM dolt_tags")
		requireOnly(t, db, "SELECT name FROM dolt_remotes")
		requireOnly(t, db, "SELECT name FROM dolt_stashes")
		requireOnly(t, db, "SELECT CONCAT(table_name, ' ', status) FROM dolt_status")

		// The pool lends the same database back, and it works.
		again, err := p.acquire(leaseSQL, "TestReuser", "")
		if err != nil {
			t.Fatal(err)
		}
		if again != e {
			t.Fatalf("re-lease got %s, want the returned %s", again.name, e.name)
		}
		mustExec(t, db, "CREATE TABLE reused (id INT PRIMARY KEY)")
		mustExec(t, db, "INSERT INTO reused VALUES (1)")
		requireCount(t, db, "SELECT COUNT(*) FROM reused", 1)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := p.release(again); err != nil {
			t.Fatal(err)
		}
	})

	// A reset point that keeps an AUTO_INCREMENT table is refused, naming the
	// column and the lessee (checkDoltDatabaseAt).
	t.Run("a reset point with an AUTO_INCREMENT table is refused", func(t *testing.T) {
		e, err := p.acquire(leaseSQL, "TestCounter", "")
		if err != nil {
			t.Fatal(err)
		}
		db := openPoolSQL(t, port, e.name)
		defer db.Close()
		mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
		mustExec(t, db, "CALL DOLT_COMMIT('-Am', 'reset point with a counter')")
		if err := p.recordHead(e); err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, "INSERT INTO ai (v) VALUES (1), (2), (3)")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		err = p.release(e)
		if err == nil || !strings.Contains(err.Error(), "AUTO_INCREMENT") || !strings.Contains(err.Error(), "TestCounter") {
			t.Fatalf("release = %v, want a refusal naming the AUTO_INCREMENT column and the lessee", err)
		}
		if e.broken == nil {
			t.Error("the database was put back in the pool after a refusal")
		}
	})

	// A lessee that left a session open is refused, and its in-flight
	// transaction cannot commit into the database the next lessee would get
	// (endSessionsOn).
	t.Run("a lessee that left a session open is refused", func(t *testing.T) {
		e, err := p.acquire(leaseSQL, "TestLeaky", "")
		if err != nil {
			t.Fatal(err)
		}
		db := openPoolSQL(t, port, e.name)
		defer db.Close()
		mustExec(t, db, "CREATE TABLE late (id INT PRIMARY KEY)")
		mustExec(t, db, "CALL DOLT_COMMIT('-Am', 'late table')")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		leaker := openPoolSQL(t, port, e.name)
		defer leaker.Close()
		tx, err := leaker.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO late VALUES (1)"); err != nil {
			t.Fatal(err)
		}

		// A second is plenty for the release to see that this session does not
		// end on its own, and keeps the case quick.
		p.sessionWait = time.Second
		err = p.release(e)
		if err == nil || !strings.Contains(err.Error(), "TestLeaky") || !strings.Contains(err.Error(), "open") {
			t.Fatalf("release with a session still open = %v, want a refusal naming the lessee", err)
		}
		if e.broken == nil {
			t.Error("the database was put back in the pool after its lessee left a session open")
		}
		// The killed session's transaction cannot commit into the database the
		// next lessee would get.
		if commitErr := tx.Commit(); commitErr == nil {
			t.Fatal("the leaked transaction committed after the release; the session must be ended first")
		}
	})
}

// openPoolSQL opens a session pool on database name of the Dolt server at
// port, with no idle connection: a connection left idle on a database fails
// its release as a session the lessee never closed (endSessionsOn).
func openPoolSQL(t *testing.T, port, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/"+name+"?timeout=30s")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	return db
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
