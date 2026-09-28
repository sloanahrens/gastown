package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"
	"testing"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

// beadsTestModeDatabase is the database name beadsdk.Open picks for dbPath
// under BEADS_TEST_MODE=1: "testdb_" plus the FNV-64a hash of the path
// (beads internal/storage/dolt applyConfigDefaults). OpenTestStore pins the
// agreement by checking, after the open, that the store's tables landed in it.
func beadsTestModeDatabase(dbPath string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(dbPath))
	return fmt.Sprintf("testdb_%x", h.Sum64())
}

// OpenTestStore opens an in-process beads store at dbPath on the shared test
// Dolt container (BEADS_TEST_MODE=1, BEADS_DOLT_PORT set by the package's
// TestMain) and fails t on any error — a store that cannot open is lost
// coverage, never a skip.
//
// beadsdk.Open would CREATE its database itself, in the middle of whatever
// other tests are migrating on the same server, and a catalog change fails
// their information_schema reads (beads test_container_catalog.go). So the
// database is created first under the exclusive side of the catalog gate, and
// the open — a no-op CREATE IF NOT EXISTS plus the schema migration — runs
// under the shared side. The database is dropped, through the gate, when t
// ends.
func OpenTestStore(t testing.TB, ctx context.Context, dbPath string) beadsdk.Storage {
	t.Helper()
	port, err := strconv.Atoi(DoltContainerPort())
	if err != nil || port == 0 {
		t.Fatalf("OpenTestStore: no shared test Dolt container (port %q)", DoltContainerPort())
	}
	name := beadsTestModeDatabase(dbPath)
	if err := beads.CreateTestDatabase(port, name); err != nil {
		t.Fatalf("OpenTestStore: create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := beads.ReleaseTestDatabase(port, name); err != nil {
			t.Logf("cleanup: drop test database %s: %v", name, err)
		}
	})
	var store beadsdk.Storage
	if err := beads.WithSharedTestCatalog(func() error {
		var openErr error
		store, openErr = beadsdk.Open(ctx, dbPath)
		return openErr
	}); err != nil {
		t.Fatalf("OpenTestStore: open beads store at %s (database %s): %v", dbPath, name, err)
	}
	// Registered after the drop, so it runs first: the store closes before
	// its database goes.
	t.Cleanup(func() { _ = store.Close() })
	if err := requireTablesIn(port, name); err != nil {
		t.Fatalf("OpenTestStore: beadsdk.Open did not migrate the database the gate created: %v", err)
	}
	return store
}

// requireTablesIn fails unless database name on the test container has tables.
// If beadsdk ever derives its test database name differently, the store opens
// a database of its own — created outside the gate — and this says so.
func requireTablesIn(port int, name string) error {
	return beads.WithSharedTestCatalog(func() error {
		db, err := sql.Open("mysql", beads.TestDatabaseDSN(port))
		if err != nil {
			return err
		}
		defer db.Close()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?", name).Scan(&n); err != nil {
			return fmt.Errorf("count tables in %s: %w", name, err)
		}
		if n == 0 {
			return fmt.Errorf("database %s has no tables", name)
		}
		return nil
	})
}
