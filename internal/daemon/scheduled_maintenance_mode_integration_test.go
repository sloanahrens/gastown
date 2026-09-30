//go:build integration

package daemon

import (
	"database/sql"
	"strings"
	"testing"
)

// TestIntegrationScheduledMaintenanceMonitorNeverFlattens runs the monitor
// case of scheduled_maintenance_mode_test.go against a real database on the
// package's Dolt container, so the dolt_log count the unit tests replace with
// countCommitsFn is the server's own: the database carries one commit, the
// threshold is 1, and monitor mode must escalate and never run gt maintain.
func TestIntegrationScheduledMaintenanceMonitorNeverFlattens(t *testing.T) {
	d := testDoltServerDaemon(t)
	dbName := createTestDB(t)

	conn, err := d.compactorOpenDB(dbName)
	if err != nil {
		t.Fatalf("open %s on the test container: %v", dbName, err)
	}
	mustExec(t, conn, "CREATE TABLE probe (id INT PRIMARY KEY)")
	mustExec(t, conn, "CALL DOLT_ADD('-A')")
	mustExec(t, conn, "CALL DOLT_COMMIT('-m','probe','--author','gt-test <gt-test@localhost>')")
	_ = conn.Close()

	escalations, execs := withMaintenanceSeams(t, d)
	runMaintenanceNow(t, d, dbName, MaintenanceModeMonitor)

	if *execs != 0 {
		t.Errorf("monitor mode ran gt maintain %d time(s) — a database's history was rewritten by a patrol meant only to observe", *execs)
	}
	if len(*escalations) != 1 {
		t.Fatalf("monitor mode escalated %d time(s), want 1 (escalations: %v)", len(*escalations), *escalations)
	}
	if !strings.Contains((*escalations)[0], dbName) {
		t.Errorf("escalation does not name the database it is about (%s):\n%s", dbName, (*escalations)[0])
	}
}

func mustExec(t *testing.T, conn *sql.DB, query string) {
	t.Helper()
	if _, err := conn.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
