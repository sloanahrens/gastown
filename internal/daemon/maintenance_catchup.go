package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
)

// maintenanceCatchUpAge is how old the newest Dolt backup may be, outside the
// maintenance window, before the next check takes a catch-up. The nightly
// window is the only other path to a backup, so a daemon that is down at 03:00
// skips a day; a day plus the window's own length is the first moment one was
// certainly missed (gt-wne04).
const maintenanceCatchUpAge = 24*time.Hour + maintenanceWindowLength

// maintenanceCatchUpRetry is the shortest gap between two catch-up attempts.
// The in-window path escalates a failed backup once and stops until the next
// window; outside it the check fires every 5 minutes, so without this a
// failing backup would rerun and re-escalate on every tick.
const maintenanceCatchUpRetry = 6 * time.Hour

// maybeCatchUpBackup takes a missed nightly backup outside the window: the
// backup only, never the gc, which stays in the window behind the full quiet
// guard. It waits for a moment when the daemon has no work in flight and no
// container-gate slot is held, so it never runs beside a landing gate; a
// working polecat does not block it, or a busy town would never catch up. It
// runs on the gc cycle's goroutine and flag, so it cannot overlap a window run.
func (d *Daemon) maybeCatchUpBackup(now time.Time) {
	if d.maintenanceGCRunning.Load() {
		return
	}
	if last := d.catchUpAttemptAt.Load(); last != 0 && now.Sub(time.Unix(0, last)) < maintenanceCatchUpRetry {
		return
	}
	s := d.maintenance()
	root, err := s.backupRoot()
	if err != nil {
		d.catchUpUnknown(now, fmt.Sprintf("backup root: %v", err))
		return
	}
	newest, ok, err := doltbackup.Newest(root)
	if err != nil {
		d.catchUpUnknown(now, fmt.Sprintf("reading backups: %v", err))
		return
	}
	if ok && newest.Age(now) < maintenanceCatchUpAge {
		return
	}
	if quiet, why := d.catchUpQuiet(); !quiet {
		d.logger.Printf("scheduled_maintenance: catch-up backup deferred: %s", why)
		return
	}
	if external, why := s.gcExternal(d); external {
		// CALL dolt_backup writes file:// on the server's host, not this one.
		d.logger.Printf("scheduled_maintenance: catch-up skipped: %s — backup needs a local server", why)
		return
	}
	databases, err := s.gcDatabases(d.maintenanceDataDir())
	if err != nil || len(databases) == 0 {
		d.logger.Printf("scheduled_maintenance: catch-up: no databases discovered (err=%v)", err)
		return
	}
	if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
		return
	}
	d.catchUpAttemptAt.Store(now.UnixNano())
	age := "none on disk"
	if ok {
		age = newest.Age(now).Round(time.Minute).String() + " old"
	}
	d.logger.Printf("scheduled_maintenance: catch-up backup outside the window (newest backup %s), %d database(s)",
		age, len(databases))
	s.dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res := d.maintenanceBackup(databases)
		d.logger.Printf("scheduled_maintenance: catch-up backup %s", res.outcome)
	})
}

// catchUpUnknown reports a backup age the check could not read. That is not
// "a recent backup exists": the gap this change closes could be open, so it
// escalates (once per retry gap) instead of returning quietly.
func (d *Daemon) catchUpUnknown(now time.Time, what string) {
	d.catchUpAttemptAt.Store(now.UnixNano())
	d.logger.Printf("scheduled_maintenance: catch-up: cannot tell the newest backup's age: %s", what)
	d.maintenance().escalate(d, "scheduled_maintenance", fmt.Sprintf(
		"scheduled_maintenance: cannot read the Dolt backups, so a missed nightly backup cannot be caught up: %s", what))
}

// catchUpQuiet is the catch-up's guard: no daemon work in flight (a landing
// pass, a dispatch, an install) and no container-gate slot held.
func (d *Daemon) catchUpQuiet() (bool, string) {
	if !d.daemonWorkIdle() {
		return false, "daemon has work in flight"
	}
	holders, err := d.maintenance().slotHolders(d.config.TownRoot)
	if err != nil {
		return false, fmt.Sprintf("cannot read slot status: %v", err)
	}
	if len(holders) > 0 {
		return false, "container-gate slot held by " + strings.Join(holders, ", ")
	}
	return true, ""
}
