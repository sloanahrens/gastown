package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	beadsdk "github.com/steveyegge/beads"
)

// OpenTestStore opens a fresh in-process beads store on the shared test Dolt
// container (BEADS_TEST_MODE=1 and BEADS_DOLT_PORT set by the package's
// TestMain). Container tests are opt-in: without GT_TEST_DOCKER=1, or without
// Docker, it skips as RequireDoltContainer does. Once opted in, any error fails
// t — a store that cannot open is lost coverage, never a skip.
//
// beadsdk.Open would CREATE its own database, in the middle of whatever other
// tests are migrating on the same server; the store path comes from the
// container's pre-created pool instead (doltpool.go), so the open only
// migrates a database that already exists.
func OpenTestStore(t *testing.T, ctx context.Context) beadsdk.Storage {
	t.Helper()
	RequireDoltContainer(t)
	dbPath, err := takeDoltPoolPath()
	if err != nil {
		t.Fatalf("OpenTestStore: %v", err)
	}
	name := beadsTestModeDatabase(dbPath)
	store, err := beadsdk.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenTestStore: open beads store at %s (database %s): %v", dbPath, name, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := requireTablesIn(name); err != nil {
		t.Fatalf("OpenTestStore: beadsdk.Open did not migrate the pooled database: %v", err)
	}
	return store
}

// requireTablesIn fails unless database name on the shared container has
// tables. If beadsdk ever derives its test database name differently, the
// store opens — and CREATEs, mid-run — a database of its own, and this says so.
func requireTablesIn(name string) error {
	port, err := strconv.Atoi(DoltContainerPort())
	if err != nil {
		return err
	}
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
