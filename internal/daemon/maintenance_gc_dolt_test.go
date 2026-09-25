package daemon

import (
	"context"
	"fmt"
	"testing"
)

// TestDoltGCFullAgainstRealServer runs doltGCFull's real SQL — the DSN, the
// CALL dolt_gc('--full') statement and its result handling — against the
// package's ephemeral Dolt container. Every other gc test stubs
// maintenanceGCExecFn, so this is the only test that proves the call works
// on a server and that history survives it. Skips when the container is
// unavailable (GT_TEST_DOCKER=0 or no Docker).
func TestDoltGCFullAgainstRealServer(t *testing.T) {
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t, d)

	conn, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testDoltSQLTimeout)
	defer cancel()
	exec := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Several commits, then a delete, so the gc has both history to keep
	// and chunks to collect.
	exec("CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(64))")
	for i := 0; i < 3; i++ {
		exec(fmt.Sprintf("INSERT INTO t VALUES (%d, 'row-%d')", i, i))
		exec(fmt.Sprintf("CALL DOLT_COMMIT('-Am', 'row %d')", i))
	}
	exec("DELETE FROM t WHERE id = 0")
	exec("CALL DOLT_COMMIT('-Am', 'drop row 0')")

	var commitsBefore int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log").Scan(&commitsBefore); err != nil {
		t.Fatalf("count dolt_log before gc: %v", err)
	}
	conn.Close()

	gcCtx, gcCancel := context.WithTimeout(context.Background(), maintenanceGCTimeout)
	defer gcCancel()
	if err := d.doltGCFull(gcCtx, dbName); err != nil {
		t.Fatalf("doltGCFull(%s) = %v, want nil", dbName, err)
	}

	// A fresh connection: the gc may invalidate the session that ran it.
	after, err := d.openDoltDB(dbName)
	if err != nil {
		t.Fatalf("reconnect to %s: %v", dbName, err)
	}
	defer after.Close()
	var commitsAfter, rows int
	if err := after.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log").Scan(&commitsAfter); err != nil {
		t.Fatalf("count dolt_log after gc: %v", err)
	}
	if commitsAfter != commitsBefore {
		t.Errorf("dolt_log has %d commits after gc, want %d (gc must keep history)", commitsAfter, commitsBefore)
	}
	if err := after.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&rows); err != nil {
		t.Fatalf("count rows after gc: %v", err)
	}
	if rows != 2 {
		t.Errorf("table t has %d rows after gc, want 2", rows)
	}
}
