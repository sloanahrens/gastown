package daemon

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// The two branches below differ only in mode, and the difference is the whole
// point of the mode: monitor reports a database over threshold, flatten
// rewrites its commit history. These tests drive runScheduledMaintenance
// itself, against a real database over a real threshold, because the failure
// they guard against — monitor mode reaching the destructive path — is a
// wiring bug that a unit test of maintenanceMode cannot see.

// TestScheduledMaintenanceMonitorNeverFlattens is the load-bearing case: the
// default mode must escalate with the counts and never run `gt maintain`.
func TestScheduledMaintenanceMonitorNeverFlattens(t *testing.T) {
	d, dbName := maintenanceTestDaemon(t)
	escalations, execs := withMaintenanceSeams(t)

	runMaintenanceNow(t, d, dbName, MaintenanceModeMonitor)

	if *execs != 0 {
		t.Errorf("monitor mode ran gt maintain %d time(s) — a database's history was rewritten by a patrol meant only to observe", *execs)
	}
	if len(*escalations) != 1 {
		t.Fatalf("monitor mode escalated %d time(s), want 1 (escalations: %v)", len(*escalations), *escalations)
	}
	msg := (*escalations)[0]
	if !strings.Contains(msg, dbName) {
		t.Errorf("escalation does not name the database it is about (%s):\n%s", dbName, msg)
	}
	// The counts are the actionable part; without them the escalation is
	// "something is over threshold, somewhere".
	if !strings.Contains(msg, "commits") {
		t.Errorf("escalation carries no commit counts:\n%s", msg)
	}
	if !strings.Contains(msg, MaintenanceModeMonitor) {
		t.Errorf("escalation does not say which mode produced it:\n%s", msg)
	}
}

// TestScheduledMaintenanceMonitorIsTheDefaultMode pins the default end to end:
// a config that never mentions a mode must still escalate rather than flatten.
func TestScheduledMaintenanceMonitorIsTheDefaultMode(t *testing.T) {
	d, dbName := maintenanceTestDaemon(t)
	escalations, execs := withMaintenanceSeams(t)

	runMaintenanceNow(t, d, dbName, "") // empty mode == unset

	if *execs != 0 {
		t.Errorf("an unset mode ran gt maintain %d time(s), want 0 — the default must be monitor", *execs)
	}
	if len(*escalations) != 1 {
		t.Fatalf("an unset mode escalated %d time(s), want 1", len(*escalations))
	}
}

// TestScheduledMaintenanceFlattenModeRunsMaintain covers the opt-in path so
// that "monitor never flattens" is not passing because flatten is broken.
func TestScheduledMaintenanceFlattenModeRunsMaintain(t *testing.T) {
	d, dbName := maintenanceTestDaemon(t)
	escalations, execs := withMaintenanceSeams(t)

	runMaintenanceNow(t, d, dbName, MaintenanceModeFlatten)

	if *execs != 1 {
		t.Fatalf("flatten mode ran gt maintain %d time(s), want 1", *execs)
	}
	if len(*escalations) != 0 {
		t.Errorf("a successful flatten escalated %v, want none", *escalations)
	}
}

// TestScheduledMaintenanceBelowThresholdIsQuiet guards the other direction: a
// database under threshold must produce neither an escalation nor an exec, in
// either mode. Without this, a patrol that escalated unconditionally would
// still pass the tests above.
func TestScheduledMaintenanceBelowThresholdIsQuiet(t *testing.T) {
	for _, mode := range []string{MaintenanceModeMonitor, MaintenanceModeFlatten} {
		t.Run(mode, func(t *testing.T) {
			d, dbName := maintenanceTestDaemon(t)
			escalations, execs := withMaintenanceSeams(t)

			// A threshold no real database can reach.
			runMaintenanceNow(t, d, dbName, mode, withMaintenanceThreshold(1_000_000))

			if *execs != 0 {
				t.Errorf("below-threshold run in %s mode ran gt maintain %d time(s), want 0", mode, *execs)
			}
			if len(*escalations) != 0 {
				t.Errorf("below-threshold run in %s mode escalated %v, want none", mode, *escalations)
			}
		})
	}
}

// --- helpers ---------------------------------------------------------------

// maintenanceTestDaemon returns a Daemon pointed at the package's ephemeral Dolt
// container, plus the name of a database on it that carries at least one commit
// (so a threshold of 1 is crossed deterministically).
func maintenanceTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()

	// testDoltRemotesDaemon is the shared constructor for a Daemon whose Dolt
	// port resolves to the package's container rather than to the live town on
	// :3307; it refuses to run if the port resolves anywhere else.
	d := testDoltRemotesDaemon(t)
	dbName := createTestDB(t, d)

	conn, err := d.compactorOpenDB(dbName)
	if err != nil {
		t.Fatalf("open %s on the test container: %v", dbName, err)
	}
	defer conn.Close()

	mustExec(t, conn, "CREATE TABLE probe (id INT PRIMARY KEY)")
	mustExec(t, conn, "CALL DOLT_ADD('-A')")
	mustExec(t, conn, "CALL DOLT_COMMIT('-m','probe','--author','gt-test <gt-test@localhost>')")

	return d, dbName
}

// maintenanceRunOptions collects the per-run knobs the tests vary.
type maintenanceRunOptions struct {
	threshold int
}

// withMaintenanceThreshold overrides the commit threshold for a run.
func withMaintenanceThreshold(n int) func(*maintenanceRunOptions) {
	return func(o *maintenanceRunOptions) { o.threshold = n }
}

// runMaintenanceNow points the patrol's window at the current hour, wires the
// config to the one test database, and runs a cycle.
func runMaintenanceNow(t *testing.T, d *Daemon, dbName, mode string, opts ...func(*maintenanceRunOptions)) {
	t.Helper()

	o := maintenanceRunOptions{threshold: 1}
	for _, opt := range opts {
		opt(&o)
	}

	now := time.Now()
	// A window covering the current hour always contains now — including the
	// 23:xx case, where the window runs to midnight rather than wrapping.
	window := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location()).Format("15:04")

	threshold := o.threshold
	d.patrolConfig = &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			// Restrict discovery to the one database this test created;
			// otherwise the patrol would count every database on the shared
			// container, including other tests' in-flight ones.
			CompactorDog: &CompactorDogConfig{Databases: []string{dbName}},
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled:   true,
				Window:    window,
				Interval:  "daily",
				Threshold: &threshold,
				Mode:      mode,
			},
		},
	}
	d.lastMaintenanceRun = time.Time{}

	d.runScheduledMaintenance()
}

// withMaintenanceSeams replaces the patrol's two side effects with recorders
// and returns pointers to the recordings: escalations as "<source>|<message>",
// and a count of gt maintain invocations.
func withMaintenanceSeams(t *testing.T) (escalations *[]string, execs *int) {
	t.Helper()

	var esc []string
	var n int

	prevEscalate, prevExec := maintenanceEscalateFn, maintenanceExecFn
	maintenanceEscalateFn = func(_ *Daemon, source, message string) {
		esc = append(esc, source+"|"+message)
	}
	maintenanceExecFn = func(context.Context, string, string, int) ([]byte, error) {
		n++
		return nil, nil
	}
	t.Cleanup(func() {
		maintenanceEscalateFn, maintenanceExecFn = prevEscalate, prevExec
	})

	return &esc, &n
}

func mustExec(t *testing.T, conn *sql.DB, query string) {
	t.Helper()
	if _, err := conn.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
