package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
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

// The catch-up check and the dispatcher's tick fire into the same pass of the
// run loop (gt-y6ovz: on 2026-10-05 every deferred check at 19:30:59, 19:36:13,
// 19:40:59 and 19:46:05 had a spec_dispatch tick in flight, three of them
// parked by the operator hold). A parked tick must not defer a backup that is
// over a day old; the ticker starts none at all.
func TestBackupCatchUpRunsWithTheDispatcherParked(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.ctx = context.Background()
	writeOperatorHold(t, d.config.TownRoot)
	release := make(chan struct{})
	fake := newFakeCLIFor(func(cliCall) cliReply {
		<-release
		return cliReply{stdout: `{"roster": "deepseek-flash 1/3", "candidates": 0}`}
	})
	d.execCmd = fake.run
	defer func() {
		close(release)
		d.specDispatchCycles.Wait()
	}()

	// The maintenance tick and this tick arrive together; whatever the tick
	// does here, it is in flight while the check runs.
	d.triggerSpecDispatch()
	d.runScheduledMaintenance()

	if len(f.backupCalls) != 2 {
		t.Errorf("backup calls = %v, want gt and hq: a parked dispatcher deferred a day-old catch-up", f.backupCalls)
	}
}

// The deferred line names the hold: "work in flight" alone left an operator
// to guess the holder from the log timeline (gt-y6ovz).
func TestBackupCatchUpDeferralNamesTheHold(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	var buf bytes.Buffer
	d.logger = log.New(&buf, "", 0)
	d.landingStates.setBead("gastown", "gt-a", catchUpNow)

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 {
		t.Fatalf("catch-up ran while a landing pass held the gate: %v", f.backupCalls)
	}
	if want := "catch-up backup deferred: daemon has work in flight (landing pass gt-a)"; !strings.Contains(buf.String(), want) {
		t.Errorf("deferred line = %q, want it to carry %q", buf.String(), want)
	}
}

// A failing catch-up escalates once and is not retried on the next 5-minute
// ticks; the retry gap passing allows exactly one more attempt.
func TestBackupCatchUpFailureIsThrottled(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	f.backupErr["gt"] = errors.New("disk full")

	d.runScheduledMaintenance()
	first := len(f.backupCalls)
	if first == 0 || len(f.escalations) != 1 {
		t.Fatalf("calls=%v escalations=%v, want an attempt and one escalation", f.backupCalls, f.escalations)
	}

	d.maint.now = func() time.Time { return catchUpNow.Add(5 * time.Minute) }
	d.runScheduledMaintenance()
	if len(f.backupCalls) != first || len(f.escalations) != 1 {
		t.Errorf("retried within the gap: calls=%v escalations=%v", f.backupCalls, f.escalations)
	}

	d.maint.now = func() time.Time { return catchUpNow.Add(maintenanceCatchUpRetry + time.Minute) }
	d.runScheduledMaintenance()
	if len(f.backupCalls) <= first || len(f.escalations) != 2 {
		t.Errorf("no retry after the gap: calls=%v escalations=%v", f.backupCalls, f.escalations)
	}
}

// An unreadable backup root is not "a recent backup exists": it escalates, once
// per retry gap, and takes no backup.
func TestBackupCatchUpUnreadableRootEscalates(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.maint.backupRoot = func() (string, error) { return "", errors.New("no home") }

	d.runScheduledMaintenance()
	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 {
		t.Errorf("took a backup with an unreadable root: %v", f.backupCalls)
	}
	if len(f.escalations) != 1 {
		t.Errorf("escalations = %v, want exactly 1", f.escalations)
	}
}

// A malformed window reads as "outside"; it must not enable catch-ups.
func TestBackupCatchUpIgnoredWithMalformedWindow(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.patrolConfig.Patrols.ScheduledMaintenance.Window = "not-a-window"

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 {
		t.Errorf("catch-up ran with a malformed window: %v", f.backupCalls)
	}
}
