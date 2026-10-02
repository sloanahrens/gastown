package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
)

// catchUpNow is mid-afternoon the day after gcTestNow's window: outside the
// 03:00 window, with no backup taken since the daemon was down overnight
// (gt-wne04).
var catchUpNow = time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)

func catchUpDaemon(t *testing.T) (*Daemon, *gcFakes) {
	t.Helper()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
	d.patrolConfig = gcModeConfig()
	d.maint.now = func() time.Time { return catchUpNow }
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	return d, f
}

// A daemon that was down across the window takes the missed backup on the next
// check outside it: the backup only, never the gc, which stays in the window.
func TestBackupCatchUpTakesMissedBackupOutsideWindow(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 2 {
		t.Fatalf("backup calls = %v, want gt and hq", f.backupCalls)
	}
	for _, e := range f.events {
		if strings.HasPrefix(e, "gc ") {
			t.Errorf("catch-up ran a gc outside the window: %v", f.events)
		}
	}
	b, ok, err := doltbackup.Newest(f.backupRoot)
	if err != nil || !ok {
		t.Fatalf("no backup committed: %v", err)
	}
	if b.Path != doltbackup.Dir(f.backupRoot, catchUpNow) {
		t.Errorf("backup = %s, want today's %s", b.Path, doltbackup.Dir(f.backupRoot, catchUpNow))
	}

	// One catch-up: the next check finds today's backup and takes nothing.
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 2 {
		t.Errorf("second check took another backup: %v", f.backupCalls)
	}
}

// A backup inside the last day means nothing was missed.
func TestBackupCatchUpSkipsWhenABackupIsRecent(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.maint.now = func() time.Time { return catchUpNow.Add(-2 * time.Hour) }
	d.maintenanceBackup([]string{"gt", "hq"})
	calls := len(f.backupCalls)
	d.maint.now = func() time.Time { return catchUpNow }

	d.runScheduledMaintenance()

	if len(f.backupCalls) != calls {
		t.Errorf("catch-up ran with a 2h-old backup: %v", f.backupCalls)
	}
}

// A held container-gate slot (a landing gate, a polecat suite) defers the
// catch-up; the next quiet check takes it.
func TestBackupCatchUpWaitsForAQuietTown(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.maint.slotHolders = func(string) ([]string, error) { return []string{"gastown/landing"}, nil }

	d.runScheduledMaintenance()
	if len(f.backupCalls) != 0 {
		t.Fatalf("catch-up ran while a slot was held: %v", f.backupCalls)
	}

	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 2 {
		t.Errorf("catch-up not taken once the town went quiet: %v", f.backupCalls)
	}
}
