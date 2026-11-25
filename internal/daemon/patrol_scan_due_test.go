package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPatrolScanDue pins the patrol_scan due-ness gate (gt-lc0ey5): the tick
// runs when the persisted last run is at least one interval old, or when no
// readable record says otherwise, and a broken record runs it with a warning.
func TestPatrolScanDue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 24, 17, 0, 0, 0, time.UTC)
	const interval = 10 * time.Minute

	for _, tc := range []struct {
		name string
		// setup writes the last-run record under townRoot, or leaves it absent.
		setup    func(t *testing.T, townRoot string)
		wantDue  bool
		wantWarn bool
	}{
		{
			name: "record 3m old at a 10m interval is not due",
			setup: func(t *testing.T, townRoot string) {
				savePatrolLastRunForTest(t, townRoot, now.Add(-3*time.Minute))
			},
			wantDue: false,
		},
		{
			name: "record 12m old at a 10m interval is due",
			setup: func(t *testing.T, townRoot string) {
				savePatrolLastRunForTest(t, townRoot, now.Add(-12*time.Minute))
			},
			wantDue: true,
		},
		{
			name:    "no record is due",
			setup:   func(t *testing.T, townRoot string) {},
			wantDue: true,
		},
		{
			name: "corrupt record is due with a warning",
			setup: func(t *testing.T, townRoot string) {
				if err := os.WriteFile(patrolLastRunPath(townRoot), []byte("{not json"), 0o600); err != nil {
					t.Fatalf("write corrupt record: %v", err)
				}
			},
			wantDue:  true,
			wantWarn: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			townRoot := patrolDueTown(t)
			tc.setup(t, townRoot)

			d := &Daemon{
				config: &Config{TownRoot: townRoot},
				logger: log.New(io.Discard, "", 0),
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					PatrolScan: &PatrolScanConfig{Enabled: true, IntervalStr: interval.String()},
				}},
			}

			dec := d.patrolScanDue(now)
			if dec.due != tc.wantDue {
				t.Errorf("due = %v, want %v (note %q)", dec.due, tc.wantDue, dec.note)
			}
			if got := dec.warn != ""; got != tc.wantWarn {
				t.Errorf("warn set = %v, want %v (warn %q)", got, tc.wantWarn, dec.warn)
			}
		})
	}
}

// patrolDueTown returns a private temp town whose daemon directory exists at
// 0o700 before any record is written, so the package writer's MkdirAll leaves
// it alone and the test never depends on the process umask.
func patrolDueTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(patrolLastRunPath(townRoot)), 0o700); err != nil {
		t.Fatalf("mkdir daemon dir: %v", err)
	}
	return townRoot
}

// savePatrolLastRunForTest writes a patrol_scan completion time into the
// daemon's last-run record under townRoot.
func savePatrolLastRunForTest(t *testing.T, townRoot string, at time.Time) {
	t.Helper()
	if err := savePatrolLastRun(townRoot, "patrol_scan", at); err != nil {
		t.Fatalf("save patrol_scan last run: %v", err)
	}
}
