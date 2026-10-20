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

// maintenanceCatchUpRetry is the shortest gap between two catch-up attempts,
// for the backup and for the gc. The in-window path
// escalates a failed backup once and stops until the next window; outside it
// the check fires every 5 minutes, so without this a failing backup would rerun
// and re-escalate on every tick. Each catch-up throttles on its own attempt
// timestamp, so a stuck gc cannot delay the backup.
const maintenanceCatchUpRetry = 6 * time.Hour

// maintenanceCatchUpGCAge is how stale a database's last patrol gc may be,
// outside the window, before the catch-up gc runs that database: the weekly
// interval plus a missed window of slack. A database with no recorded gc is
// due. The old-gen trigger is not consulted out of window — the option removed
// is the one that fires on growth, and the window remains where a gc runs on
// schedule (gt-uux5b).
const maintenanceCatchUpGCAge = 8 * 24 * time.Hour

// maybeCatchUpBackup takes a missed nightly backup outside the window. It waits
// for a moment when no hard daemon work is in flight and no
// container-gate slot is held, so it never runs beside a landing gate; a
// working polecat — or a soft hold the next tick reruns — does not block it, or
// a town busy with its own ticks would never catch up. It runs on the gc
// cycle's goroutine and flag, so it cannot overlap a window run.
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
		d.logCatchUpDeferral(now, why)
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
	age := "none on disk"
	if ok {
		age = newest.Age(now).Round(time.Minute).String() + " old"
	}
	d.logger.Printf("scheduled_maintenance: catch-up backup outside the window (newest backup %s), %d database(s)",
		age, len(databases))
	s.dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res := d.maintenanceBackup(databases)
		if res.outcome == gcOutcomeDeferred {
			// A held Dolt lock or a pause marker means no backup was tried:
			// the next 5-minute tick retries, so the throttle stays reserved
			// for a run that completed or failed, as the catch-up gc's is.
			// Only the retry is unthrottled; the line saying so is logged once
			// per reason per retry gap.
			d.logCatchUpDeferral(now, res.reason)
			return
		}
		d.clearCatchUpDeferral()
		d.logger.Printf("scheduled_maintenance: catch-up backup %s", res.outcome)
		d.catchUpAttemptAt.Store(now.UnixNano())
	})
}

// logCatchUpDeferral logs why the catch-up backup waits, once per reason per
// maintenanceCatchUpRetry. A deferral retries on every 5-minute tick by design,
// so a hold that lasts hours would otherwise write a line per tick; a changed
// reason is a new fact and is logged at once.
func (d *Daemon) logCatchUpDeferral(now time.Time, why string) {
	d.catchUpDeferMu.Lock()
	defer d.catchUpDeferMu.Unlock()
	if why == d.catchUpDeferWhy && now.Sub(d.catchUpDeferLoggedAt) < maintenanceCatchUpRetry {
		return
	}
	d.catchUpDeferWhy, d.catchUpDeferLoggedAt = why, now
	d.logger.Printf("scheduled_maintenance: catch-up backup deferred: %s", why)
}

// clearCatchUpDeferral forgets the last logged deferral, so the next hold is
// logged on its first tick instead of waiting out the previous one's gap.
func (d *Daemon) clearCatchUpDeferral() {
	d.catchUpDeferMu.Lock()
	d.catchUpDeferWhy = ""
	d.catchUpDeferMu.Unlock()
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

// catchUpGCDue lists the databases a catch-up gc should run: those with no
// recorded gc, or a last one older than maintenanceCatchUpGCAge. The weekly and
// old-gen triggers are not consulted — a database this stale is already past
// the weekly interval, and growth alone is what the window is for.
func catchUpGCDue(now time.Time, databases []string, st maintenanceGCState) []string {
	var due []string
	for _, db := range databases {
		if last := st.LastGC[db]; last.IsZero() || now.Sub(last) >= maintenanceCatchUpGCAge {
			due = append(due, db)
		}
	}
	return due
}

// catchUpBackedUp splits the due databases into the ones a catch-up gc may run
// and the ones it may not. A database is ready only when a backup holds it and
// that backup is younger than maintenanceCatchUpAge: the age applied to the
// newest backup says a backup ran, not that it holds the database in question.
// Only the catch-up has that gap — the window takes its backup from the
// databases it is about to gc — and the catch-up reuses a backup that may
// predate a database created since, so gc'ing one with no backup of it is
// exactly what the backup-first rule exists to prevent (gt-i95fv). A backup
// that is absent, one past the age and one holding something else all land in
// unbacked: skip that database and let the rest of the pass run.
func catchUpBackedUp(now time.Time, root string, due []string) (ready, unbacked []string) {
	for _, db := range due {
		b, ok, err := doltbackup.LastFor(root, db)
		if err != nil || !ok || b.Age(now) >= maintenanceCatchUpAge {
			unbacked = append(unbacked, db)
			continue
		}
		ready = append(ready, db)
	}
	return ready, unbacked
}

// maybeCatchUpGC runs the gc outside the window for the databases whose last gc
// is older than maintenanceCatchUpGCAge (gt-uux5b). It exists for the town
// whose windows kept deferring — 03:00 is the hour the town is asleep or down,
// and a database can wait weeks on "daemon has work in flight" alone — so it
// runs the very cycle the window runs (maintenanceGCCycle, unchanged) once the
// hard-hold guard the window's cycle re-checks per database allows it and a
// backup younger than maintenanceCatchUpAge is on disk. That age is the whole
// backup's, not one database's: the gc is per database, so each database it
// takes also needs a backup holding it (catchUpBackedUp, gt-i95fv), and one the
// backup does not hold — created since the last nightly backup — waits for a
// pass that has one.
// The backup still comes first: the gc never runs without one.
//
// Fail closed. A window that does not parse, an external server, an unreadable
// state file, a quiet probe that cannot answer or a backup older than the
// catch-up age all mean no gc; the check fires every 5 minutes, so a deferral
// simply retries on a later tick. Only a cycle that ran to its end sets the
// retry gap, and only a failed one escalates (maintenanceGCCycle does that).
// Nothing here counts toward the skipped-window streak and nothing sets the
// cycle-completion time the window folds into lastMaintenanceRun: the next
// window still takes its own backup and runs its own gc.
func (d *Daemon) maybeCatchUpGC(now time.Time) {
	if d.maintenanceGCRunning.Load() {
		// A window run, a catch-up backup or a catch-up gc holds the cycle.
		return
	}
	if last := d.catchUpGCAttemptAt.Load(); last != 0 && now.Sub(time.Unix(0, last)) < maintenanceCatchUpRetry {
		return
	}
	// Every condition below is one maybeCatchUpBackup looks at on this same
	// tick and reports itself — a backup root it cannot read, a backup list it
	// cannot parse, a backup too old, an external server, no databases — so the
	// gc fails closed in silence rather than repeating it every 5 minutes.
	s := d.maintenance()
	root, err := s.backupRoot()
	if err != nil {
		return
	}
	newest, ok, err := doltbackup.Newest(root)
	if err != nil {
		return
	}
	if !ok || newest.Age(now) >= maintenanceCatchUpAge {
		// The catch-up backup above is what puts one there; in production that
		// runs on its own goroutine, so the gc waits for a later tick.
		return
	}
	if external, _ := s.gcExternal(d); external {
		return
	}
	dataDir := d.maintenanceDataDir()
	databases, err := s.gcDatabases(dataDir)
	if err != nil || len(databases) == 0 {
		return
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		// Unlike the window's cycle, which treats unreadable state as "never
		// gc'd", the catch-up cannot tell which databases are due: no gc.
		d.logger.Printf("scheduled_maintenance: catch-up gc skipped: cannot read %s: %v", maintenanceGCStateFileName, err)
		return
	}
	due := catchUpGCDue(now, databases, st)
	if len(due) == 0 {
		return // nothing stale; this check fires every 5 minutes
	}
	// A fresh backup on disk is not a backup of a database created after it: a
	// database no backup holds is logged and skipped, and the rest still run
	// (gt-i95fv).
	due, unbacked := catchUpBackedUp(now, root, due)
	if len(unbacked) > 0 {
		d.logger.Printf("scheduled_maintenance: catch-up gc skips %d database(s) no backup younger than %v holds: %s",
			len(unbacked), maintenanceCatchUpAge, strings.Join(unbacked, ", "))
	}
	if len(due) == 0 {
		return
	}
	// The full guard, asked once before the cycle so a busy town is logged as
	// waiting rather than as a dispatched cycle that only deferred; the cycle
	// re-checks it per database.
	if quiet, why := s.quiet(d); !quiet {
		d.logger.Printf("scheduled_maintenance: catch-up gc deferred: %s", why)
		return
	}
	if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
		return
	}
	d.logger.Printf("scheduled_maintenance: catch-up gc outside the window: %d database(s) past %v since their last gc: %s",
		len(due), maintenanceCatchUpGCAge, strings.Join(due, ", "))
	s.dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res := d.maintenanceGCCycle(due, dataDir)
		d.logger.Printf("scheduled_maintenance: catch-up gc cycle %s", res.outcome)
		if res.outcome == gcOutcomeDeferred {
			// The town went busy behind the guard. The next 5-minute tick
			// retries, as the window's own deferral does, so the throttle
			// stays reserved for a run that failed.
			return
		}
		if res.outcome == gcOutcomeInterrupted {
			// A shutdown is not an attempt: leave the throttle alone.
			return
		}
		d.catchUpGCAttemptAt.Store(d.maintenance().now().UnixNano())
	})
}

// catchUpQuiet is the catch-up's guard: no hard daemon work in flight (a
// landing pass, a scheduled slings cycle, an install) and no
// container-gate slot held. It names the hold it found: the deferred line is
// the only record of why a day-old backup waited, and a bare "work in flight"
// left the holder to be guessed from the log timeline (gt-y6ovz).
//
// Soft work is not a hold (gt-2m8sj): see catchUpWorkHold. The window gc's own
// guard (maintenanceQuiet) asks this same question with the same predicate — a
// polecat working or soft work in flight does not defer a gc of seconds either
// (gt-mdejo) — and the two lines differ only in the wording each caller logs.
func (d *Daemon) catchUpQuiet() (bool, string) {
	if hold := d.catchUpWorkHold(); hold != nil {
		return false, "daemon has work in flight (" + hold.name + ")"
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
