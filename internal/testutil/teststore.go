package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	beadsdk "github.com/steveyegge/beads"
)

// OpenTestStore opens an in-process beads store on the shared test Dolt
// container (BEADS_TEST_MODE=1 and BEADS_DOLT_PORT set by the package's
// TestMain). Container tests are opt-in: without GT_TEST_DOCKER=1, or without
// Docker, it skips as RequireDoltContainer does. Once opted in, any error fails
// t — a store that cannot open is lost coverage, never a skip.
//
// beadsdk.Open would CREATE its own database, in the middle of whatever other
// tests are migrating on the same server; the store path comes from the
// container's pool instead (doltpool.go), so the open only migrates a database
// that already exists — or, when an earlier test already migrated it, finds
// it migrated. The store is t's alone until t ends; then it is closed, and its
// database is reset and goes back to the pool.
func OpenTestStore(t *testing.T, ctx context.Context) beadsdk.Storage {
	t.Helper()
	RequireDoltContainer(t)
	p := currentDoltPool()
	if p == nil {
		t.Fatal("OpenTestStore: the shared test Dolt container has no pool")
	}
	return openPooledStore(t, ctx, p)
}

// openPooledStore opens a store on a database leased from p for t.
func openPooledStore(t *testing.T, ctx context.Context, p *doltDBPool) beadsdk.Storage {
	t.Helper()
	// The lease's cleanup is registered before the store's, so it runs after
	// the store has closed.
	e := p.leaseForTest(t, leaseStore)
	store, err := beadsdk.Open(ctx, e.path)
	if err != nil {
		t.Fatalf("OpenTestStore: open beads store at %s (database %s): %v", e.path, e.name, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := requireTablesIn(p.port, e.name); err != nil {
		t.Fatalf("OpenTestStore: beadsdk.Open did not migrate the pooled database: %v", err)
	}
	if !e.migrated {
		if err := p.markMigrated(e); err != nil {
			t.Fatalf("OpenTestStore: %v", err)
		}
	}
	return store
}

// requireTablesIn fails unless database name on the server at port has
// tables. If beadsdk ever derives its test database name differently, the
// store opens — and CREATEs, mid-run — a database of its own, and this says so.
func requireTablesIn(port int, name string) error {
	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s", port))
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
}
