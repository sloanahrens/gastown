package daemon

import "testing"

// runUnrequestedTick takes the catch-ups for a window that parses and none for
// one that does not: a catch-up there would hide that the window, and its gc,
// never runs.
func TestRunUnrequestedTick(t *testing.T) {
	t.Parallel()
	// staleGC has a recent backup on disk and one stale database, so only the
	// catch-up gc has work.
	staleGC := func(t *testing.T) (*Daemon, *gcFakes) {
		d, f := catchUpGCDaemon(t)
		catchUpGCStale(t, d, "gt")
		return d, f
	}
	for _, tc := range []struct {
		name            string
		daemon          func(*testing.T) (*Daemon, *gcFakes)
		window          string
		backups, gcRuns int
	}{
		{"a missed backup, then its gc", catchUpDaemon, "03:00", 2, 2},
		{"a missed backup, malformed window", catchUpDaemon, "not-a-window", 0, 0},
		{"a stale gc", staleGC, "03:00", 0, 1},
		{"a stale gc, malformed window", staleGC, "not-a-window", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, f := tc.daemon(t)

			d.runUnrequestedTick(catchUpNow, false, tc.window)

			if len(f.backupCalls) != tc.backups {
				t.Errorf("backup calls = %v, want %d", f.backupCalls, tc.backups)
			}
			if len(f.gcCalls) != tc.gcRuns {
				t.Errorf("gc calls = %v, want %d", f.gcCalls, tc.gcRuns)
			}
		})
	}
}
