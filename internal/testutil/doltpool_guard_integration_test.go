//go:build integration && !windows

package testutil

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// TestIntegrationDoltCatalogGuardFiresOnRealServer is the guard's mutation proof against
// the real image: on a container holding the image's databases and a pool, the
// guard passes; a stray CREATE DATABASE, and a CREATE then DROP that leaves
// SHOW DATABASES as it was, each fail it by name. It runs on the scratch
// container, which no other test uses while it holds the lease, so its
// catalog changes cannot reach any other test; the lease's reset also
// proves itself here, since the guard's first check is that the catalog
// is the image's own.
func TestIntegrationDoltCatalogGuardFiresOnRealServer(t *testing.T) {
	port := LeaseScratchDoltContainer(t)
	db, err := sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/?timeout=30s")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := verifyDoltCatalog(db, nil); err != nil {
		t.Fatalf("a fresh %s container is not exactly doltImageDatabases: %v", DoltDockerImage, err)
	}
	pool := map[string]bool{"testdb_guard_a": true, "testdb_guard_b": true}
	for name := range pool {
		mustExec(t, db, "CREATE DATABASE `"+name+"`")
	}
	if err := verifyDoltCatalog(db, pool); err != nil {
		t.Fatalf("guard on an untouched pool: %v", err)
	}

	mustExec(t, db, "CREATE DATABASE `beads`")
	requireGuardNames(t, db, pool, `database "beads" was created`)

	mustExec(t, db, "DROP DATABASE `beads`")
	requireGuardNames(t, db, pool, `database "beads" was dropped (Dolt still holds it for dolt_undrop)`)

	mustExec(t, db, "CALL dolt_purge_dropped_databases()")
	if err := verifyDoltCatalog(db, pool); err != nil {
		t.Fatalf("guard after purging the stray: %v", err)
	}
	mustExec(t, db, "DROP DATABASE `testdb_guard_a`")
	requireGuardNames(t, db, pool, `database "testdb_guard_a" was dropped`)
}

func requireGuardNames(t *testing.T, db *sql.DB, pool map[string]bool, want string) {
	t.Helper()
	err := verifyDoltCatalog(db, pool)
	if !errors.Is(err, ErrDoltCatalogChanged) {
		t.Fatalf("guard = %v, want ErrDoltCatalogChanged naming %s", err, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("guard error %q does not say %s", err, want)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
