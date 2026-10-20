package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
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

// recordCatchUpGC seeds a database's last patrol gc at.
func recordCatchUpGC(t *testing.T, d *Daemon, db string, at time.Time) {
	t.Helper()
	if err := recordMaintenanceGCRun(d.config.TownRoot, db, gcMeasure{}, gcMeasure{}, at); err != nil {
		t.Fatal(err)
	}
}

// catchUpGCDaemon is catchUpDaemon with a two-hour-old backup on disk and both
// databases gc'd two hours ago: the catch-up gc has nothing to wait for but a
// stale database. A test marks one stale with catchUpGCStale.
func catchUpGCDaemon(t *testing.T) (*Daemon, *gcFakes) {
	t.Helper()
	d, f := catchUpDaemon(t)
	at := catchUpNow.Add(-2 * time.Hour)
	d.maint.now = func() time.Time { return at }
	d.maintenanceBackup([]string{"gt", "hq"})
	d.maint.now = func() time.Time { return catchUpNow }
	recordCatchUpGC(t, d, "gt", at)
	recordCatchUpGC(t, d, "hq", at)
	f.backupCalls, f.events = nil, nil
	return d, f
}

// catchUpGCStale moves db's last gc past the catch-up age.
func catchUpGCStale(t *testing.T, d *Daemon, db string) {
	t.Helper()
	recordCatchUpGC(t, d, db, catchUpNow.Add(-maintenanceCatchUpGCAge-time.Hour))
}

// A daemon that was down across the window takes the missed backup on the next
// check outside it, and a database whose windows kept deferring is gc'd after
// it (gt-uux5b).
func TestBackupCatchUpTakesMissedBackupOutsideWindow(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 2 {
		t.Fatalf("backup calls = %v, want gt and hq", f.backupCalls)
	}
	b, ok, err := doltbackup.Newest(f.backupRoot)
	if err != nil || !ok {
		t.Fatalf("no backup committed: %v", err)
	}
	if b.Path != doltbackup.Dir(f.backupRoot, catchUpNow) {
		t.Errorf("backup = %s, want today's %s", b.Path, doltbackup.Dir(f.backupRoot, catchUpNow))
	}
	// Both databases have no recorded gc, so the catch-up gc follows the
	// backup in the same pass — the backup is never the last word.
	backed, gc := -1, -1
	for i, e := range f.events {
		if backed < 0 && strings.HasPrefix(e, "backup ") {
			backed = i
		}
		if gc < 0 && strings.HasPrefix(e, "gc ") {
			gc = i
		}
	}
	if len(f.gcCalls) != 2 || backed < 0 || gc < 0 || gc < backed {
		t.Errorf("gc calls = %v after backup calls %v: the gc must follow the backup", f.gcCalls, f.backupCalls)
	}

	// One catch-up: the next check finds today's backup and a recorded gc.
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 2 {
		t.Errorf("second check took another backup: %v", f.backupCalls)
	}
	if len(f.gcCalls) != 2 {
		t.Errorf("second check gc'd again: %v", f.gcCalls)
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

// A backup is a read through the running server, so the soft work that shares
// the maintenance check's run-loop pass — a dispatch tick, a patrol scan, the
// dog cycle, a plugin run — must not defer a day-old
// catch-up (gt-2m8sj: on 2026-10-05 19:30-19:46 and 2026-10-07 09:22-09:27 five
// checks in a row deferred with a tick or a scan in flight, and the newest
// backup reached 62h).
func TestBackupCatchUpRunsBesideTicksScansAndDogCycles(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.scripts = newScriptRunner()
	if !d.scripts.tryStart("rebuild-gt") {
		t.Fatal("could not mark a plugin run in flight")
	}
	d.compactorDogMu.Lock()
	d.compactorDogRunning = true
	d.compactorDogMu.Unlock()
	d.specDispatchRunning.Store(true)
	d.patrolScanRunning.Store(true)

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 2 {
		t.Errorf("backup calls = %v, want gt and hq: soft work deferred a day-old catch-up", f.backupCalls)
	}
}

// The hard work still defers the catch-up: a backup never runs beside a
// landing pass, a slings dispatch mid-entry or a binary mid-replacement.
func TestBackupCatchUpStillWaitsForHardWork(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hold string
		set  func(t *testing.T, d *Daemon)
	}{
		{"landing pass", "landing pass gt-a", func(_ *testing.T, d *Daemon) {
			d.landingStates.setBead("gastown", "gt-a", catchUpNow)
		}},
		{"scheduled slings cycle", "a scheduled slings cycle", func(_ *testing.T, d *Daemon) {
			d.scheduledSlingsRunning.Store(true)
		}},
		{"install lock", "the install lock (an install is replacing the binary)", func(t *testing.T, d *Daemon) {
			holdInstallLock(t, writeInstallLock(t, d))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, f := catchUpDaemon(t)
			var buf bytes.Buffer
			d.logger = log.New(&buf, "", 0)
			tc.set(t, d)

			d.runScheduledMaintenance()

			if len(f.backupCalls) != 0 {
				t.Fatalf("catch-up ran while %s held the daemon: %v", tc.name, f.backupCalls)
			}
			if want := "catch-up backup deferred: daemon has work in flight (" + tc.hold + ")"; !strings.Contains(buf.String(), want) {
				t.Errorf("deferred line = %q, want it to carry %q", buf.String(), want)
			}
		})
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
	if len(f.gcCalls) != 0 {
		t.Errorf("catch-up gc ran with a malformed window: %v", f.gcCalls)
	}
}

// --- the catch-up gc (gt-uux5b) -----------------------------------------------

// Which databases a catch-up gc takes: past the catch-up age, or never gc'd.
// The window's weekly trigger is not enough out of window — a database a week
// off its gc waits for the window.
func TestCatchUpGCDue(t *testing.T) {
	t.Parallel()
	st := maintenanceGCState{LastGC: map[string]time.Time{
		"never":  {},
		"fresh":  catchUpNow.Add(-48 * time.Hour),
		"weekly": catchUpNow.Add(-maintenanceGCWeekly - time.Hour),
		"stale":  catchUpNow.Add(-maintenanceCatchUpGCAge - time.Minute),
		"exact":  catchUpNow.Add(-maintenanceCatchUpGCAge),
	}}
	got := catchUpGCDue(catchUpNow, []string{"never", "fresh", "weekly", "stale", "exact"}, st)
	if want := "never,stale,exact"; strings.Join(got, ",") != want {
		t.Errorf("catchUpGCDue = %v, want %s", got, want)
	}
}

// A database whose last gc is younger than the catch-up age is left to the
// window; marking one stale is what the catch-up takes.
func TestCatchUpGCOnlyTakesStaleDatabases(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Fatalf("gc ran with every database recently gc'd: %v", f.gcCalls)
	}

	// A week since the last gc is due in the window (the weekly trigger) and
	// not yet due to the catch-up: the age is what decides, so the same
	// database that the window would gc waits here.
	recordCatchUpGC(t, d, "gt", catchUpNow.Add(-7*24*time.Hour))
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Fatalf("gc ran on a database inside the catch-up age: %v", f.gcCalls)
	}

	catchUpGCStale(t, d, "gt")
	d.runScheduledMaintenance()
	if got := strings.Join(f.gcCalls, ","); got != "gt" {
		t.Errorf("gc calls = %q, want gt alone", got)
	}
}

// A database created after the last nightly backup is in no backup: the
// catch-up skips it for this pass, logs it, and still gc's the databases the
// backup holds (gt-i95fv).
func TestCatchUpGCSkipsADatabaseTheBackupDoesNotHold(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	var buf bytes.Buffer
	d.logger = log.New(&buf, "", 0)
	// A database that appeared after this morning's backup: on disk, never
	// gc'd, in no backup at all.
	f.sizes["newdb"] = gcMeasure{633 * mib, 0}
	catchUpGCStale(t, d, "gt")

	d.runScheduledMaintenance()

	if got := strings.Join(f.gcCalls, ","); got != "gt" {
		t.Errorf("gc calls = %q, want gt alone: no backup holds newdb", got)
	}
	var skipped string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "catch-up gc skips") {
			skipped = line
		}
	}
	if !strings.HasSuffix(skipped, ": newdb") || !strings.Contains(skipped, "1 database(s)") {
		t.Errorf("skip line = %q, want it to name newdb alone", skipped)
	}
}

// Which due databases a catch-up gc may take: only the ones a backup younger
// than the catch-up age holds. A database in no backup at all is out, one only
// an older backup holds is out once that backup is past the age, and a listing
// that cannot be read or found leaves nothing (gt-i95fv).
func TestCatchUpGCBackedUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// commitBackup writes one complete night holding databases, as the nightly
	// backup leaves it on disk.
	commitBackup := func(databases []string, at time.Time) {
		t.Helper()
		if err := os.MkdirAll(doltbackup.PartialDir(root, at), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := doltbackup.Commit(root, doltbackup.Manifest{
			Started: at, Finished: at, Databases: databases,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// This morning's backup: gt, and not the database created after it.
	commitBackup([]string{"gt"}, catchUpNow.Add(-2*time.Hour))
	// A night past the catch-up age holds "old", but is too old to cover a gc.
	commitBackup([]string{"old"}, catchUpNow.Add(-maintenanceCatchUpAge-24*time.Hour))
	// A root holding a file where a backup root belongs: no listing can read it.
	unreadable := filepath.Join(t.TempDir(), "not-a-root")
	if err := os.WriteFile(unreadable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		root     string
		due      []string
		ready    string
		unbacked string
	}{
		{"held by the fresh backup", root, []string{"gt"}, "gt", ""},
		{"in no backup", root, []string{"newdb"}, "", "newdb"},
		{"held only by a backup past the age", root, []string{"old"}, "", "old"},
		{"some held, some not", root, []string{"gt", "newdb"}, "gt", "newdb"},
		{"unreadable listing", unreadable, []string{"gt", "newdb"}, "", "gt,newdb"},
		{"missing root", filepath.Join(root, "gone"), []string{"gt"}, "", "gt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ready, unbacked := catchUpBackedUp(catchUpNow, tc.root, tc.due)
			if got := strings.Join(ready, ","); got != tc.ready {
				t.Errorf("ready = %q, want %q", got, tc.ready)
			}
			if got := strings.Join(unbacked, ","); got != tc.unbacked {
				t.Errorf("unbacked = %q, want %q", got, tc.unbacked)
			}
		})
	}
}

// The catch-up gc waits for the hard holds the window's cycle also waits for —
// a held gate slot, a landing pass, a held install lock — and for nothing else:
// soft work and a working polecat are no reason to defer a gc of seconds
// (gt-mdejo). A deferral is not recorded — the skipped-window streak counts
// windows.
func TestCatchUpGCWaitsForHardHoldsOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		busy func(t *testing.T, d *Daemon)
		gc   bool
		want string
	}{
		{"gate slot held", func(_ *testing.T, d *Daemon) {
			d.maint.slotHolders = func(string) ([]string, error) { return []string{"gastown/landing"}, nil }
		}, false, "catch-up gc deferred: slot held by gastown/landing"},
		{"landing pass in flight", func(_ *testing.T, d *Daemon) {
			d.landingStates.setBead("gastown", "gt-a", catchUpNow)
		}, false, "catch-up gc deferred: daemon has work in flight (landing pass gt-a)"},
		{"install lock held", func(t *testing.T, d *Daemon) {
			holdInstallLock(t, writeInstallLock(t, d))
		}, false, "catch-up gc deferred: daemon has work in flight (the install lock"},
		{"polecat working", func(_ *testing.T, d *Daemon) {
			d.maint.workingPolecats = func(*Daemon) ([]string, error) { return []string{"gastown/furiosa"}, nil }
		}, true, ""},
		{"soft work in flight", func(_ *testing.T, d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
			d.compactorDogMu.Lock()
			d.compactorDogRunning = true
			d.compactorDogMu.Unlock()
		}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, f := catchUpGCDaemon(t)
			// The real guard, over the seams the case sets.
			d.maint.quiet = (*Daemon).maintenanceQuiet
			var buf bytes.Buffer
			d.logger = log.New(&buf, "", 0)
			catchUpGCStale(t, d, "gt")
			tc.busy(t, d)

			d.runScheduledMaintenance()

			if got := len(f.gcCalls) > 0; got != tc.gc {
				t.Fatalf("gc ran = %v, want %v (calls %v); log: %s", got, tc.gc, f.gcCalls, buf.String())
			}
			if !tc.gc {
				if !strings.Contains(buf.String(), tc.want) {
					t.Errorf("log = %q, want it to carry %q", buf.String(), tc.want)
				}
				st, err := loadMaintenanceGCState(d.config.TownRoot)
				if err != nil {
					t.Fatal(err)
				}
				if st.PendingDeferral != nil || st.ConsecutiveDeferredWindows != 0 || len(st.DeferralReasons) != 0 {
					t.Errorf("a catch-up deferral was recorded: %+v", st)
				}
			}
		})
	}
}

// A gc that fails escalates once, and the catch-up does not rerun it on every
// 5-minute tick: its own attempt timestamp bounds it to one per retry gap, and
// the backup catch-up keeps its own.
func TestCatchUpGCErrorEscalatesOnceAndIsThrottled(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	catchUpGCStale(t, d, "gt")
	f.gcErr["gt"] = errors.New("dolt_gc --full: connection refused")

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 1 || len(f.escalations) != 1 {
		t.Fatalf("calls=%v escalations=%v, want one attempt and one escalation", f.gcCalls, f.escalations)
	}

	d.maint.now = func() time.Time { return catchUpNow.Add(5 * time.Minute) }
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 1 || len(f.escalations) != 1 {
		t.Errorf("retried within the gap: calls=%v escalations=%v", f.gcCalls, f.escalations)
	}

	d.maint.now = func() time.Time { return catchUpNow.Add(maintenanceCatchUpRetry + time.Minute) }
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 2 || len(f.escalations) != 2 {
		t.Errorf("no retry after the gap: calls=%v escalations=%v", f.gcCalls, f.escalations)
	}
	// The backup catch-up was not starved by the failing gc: its throttle is
	// its own, so a backup stale by age is still taken.
	f.backupCalls = nil
	d.maint.now = func() time.Time { return catchUpNow.Add(maintenanceCatchUpAge + time.Hour) }
	d.runScheduledMaintenance()
	if len(f.backupCalls) == 0 {
		t.Error("the failing gc blocked the backup catch-up")
	}
}

// A catch-up whose cycle defers behind the guard — the town went busy between
// the check and the per-database call — is not throttled: the next tick gives
// it the town's next quiet moment. Only a cycle that ran out waits for the gap.
func TestCatchUpGCDeferralRetriesNextTick(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	catchUpGCStale(t, d, "gt")
	f.quietUntil = 1 // quiet for the check, busy for the cycle's re-check

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Fatalf("gc ran while the town was busy: %v", f.gcCalls)
	}

	f.quietUntil = -1 // town goes quiet
	d.runScheduledMaintenance()
	if got := strings.Join(f.gcCalls, ","); got != "gt" {
		t.Errorf("after a deferral the next tick gc'd %q, want gt", got)
	}
}

// A state file the daemon cannot read is no basis for a gc: the catch-up
// cannot tell which databases are stale, so it runs none.
func TestCatchUpGCUnreadableStateRunsNoGC(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	catchUpGCStale(t, d, "gt")
	path := maintenanceGCStatePath(d.config.TownRoot)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran on an unreadable state file: %v", f.gcCalls)
	}
}

// The gc never runs without a backup younger than the catch-up age: a backup
// that failed leaves nothing on disk, and the next tick retries the backup.
func TestCatchUpGCSkipsWithoutAFreshBackup(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	f.backupErr["gt"] = errors.New("disk full")

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran with no backup on disk: %v", f.gcCalls)
	}
}

// A gc cycle already in flight (a window run, another catch-up) owns the
// databases; the catch-up stands down and takes its turn once it is clear.
func TestCatchUpGCSkipsWhileAGCCycleIsRunning(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	catchUpGCStale(t, d, "gt")
	d.maintenanceGCRunning.Store(true)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Fatalf("a second cycle started while one was running: %v", f.gcCalls)
	}

	d.maintenanceGCRunning.Store(false)
	d.runScheduledMaintenance()
	if got := strings.Join(f.gcCalls, ","); got != "gt" {
		t.Errorf("gc calls = %q, want gt once the cycle is clear", got)
	}
}

// An externally managed server, or one off this host, has a data dir this host
// cannot measure: no catch-up gc.
func TestCatchUpGCSkipsWithAnExternalServer(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	catchUpGCStale(t, d, "gt")
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran against an external server: %v", f.gcCalls)
	}
}

// A catch-up cycle cut off by the daemon's shutdown is not an attempt: it
// neither escalates nor starts the retry gap, so the next daemon's first tick
// gc's the database again.
func TestCatchUpGCInterruptedByShutdownIsNotThrottled(t *testing.T) {
	t.Parallel()
	d, f := catchUpGCDaemon(t)
	catchUpGCStale(t, d, "gt")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	d.maint.gcExec = func(ctx context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		cancel() // shutdown() cancels d.ctx while the gc runs
		return ctx.Err()
	}

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 1 || len(f.escalations) != 0 {
		t.Fatalf("calls=%v escalations=%v, want one attempt and no escalation", f.gcCalls, f.escalations)
	}
	if at := d.catchUpGCAttemptAt.Load(); at != 0 {
		t.Errorf("catchUpGCAttemptAt = %d, want 0: a shutdown is not an attempt", at)
	}

	// The next tick of a daemon that is running again, well inside the gap.
	d.ctx = context.Background()
	d.maint.now = func() time.Time { return catchUpNow.Add(5 * time.Minute) }
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		return nil
	}
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 2 {
		t.Errorf("gc calls = %v, want a second attempt on the next tick", f.gcCalls)
	}
}

// A backup the town held off (a daemon Dolt task owns doltMaintMu) was not
// tried, so the next tick retries it instead of waiting out the retry gap.
func TestBackupCatchUpDeferredBackupRetriesNextTick(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)

	d.doltMaintMu.RLock() // a daemon Dolt task in flight
	d.runScheduledMaintenance()
	d.doltMaintMu.RUnlock()
	if len(f.backupCalls) != 0 {
		t.Fatalf("backup ran while a Dolt task held the lock: %v", f.backupCalls)
	}

	d.maint.now = func() time.Time { return catchUpNow.Add(5 * time.Minute) }
	d.runScheduledMaintenance()
	if len(f.backupCalls) != 2 {
		t.Errorf("backup calls = %v, want gt and hq on the tick after the deferral", f.backupCalls)
	}
}

// A deferral the next tick retries is logged once per reason per retry gap,
// not on every 5-minute tick, and a backup that runs forgets it.
func TestBackupCatchUpDeferralLogIsThrottled(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	var buf lockedBuffer
	d.logger = log.New(&buf, "", 0)
	deferrals := func() int { return strings.Count(buf.String(), "catch-up backup deferred") }
	tick := func(after time.Duration) {
		d.maint.now = func() time.Time { return catchUpNow.Add(after) }
		d.runScheduledMaintenance()
	}

	d.doltMaintMu.RLock() // a daemon Dolt task in flight
	for _, after := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute} {
		tick(after)
	}
	if n := deferrals(); n != 1 {
		t.Errorf("%d deferral lines over three ticks, want 1:\n%s", n, buf.String())
	}
	tick(maintenanceCatchUpRetry)
	if n := deferrals(); n != 2 {
		t.Errorf("%d deferral lines after the retry gap, want 2:\n%s", n, buf.String())
	}
	d.doltMaintMu.RUnlock()
	if len(f.backupCalls) != 0 {
		t.Fatalf("backup ran while a Dolt task held the lock: %v", f.backupCalls)
	}

	// The held lock lifts: the backup runs and the next hold is logged on its
	// first tick, not after the previous reason's gap.
	tick(maintenanceCatchUpRetry + 5*time.Minute)
	if len(f.backupCalls) != 2 {
		t.Fatalf("backup calls = %v, want gt and hq once the lock lifted", f.backupCalls)
	}
	if why := d.catchUpDeferWhy; why != "" {
		t.Errorf("deferral %q still remembered after the backup ran", why)
	}
}
