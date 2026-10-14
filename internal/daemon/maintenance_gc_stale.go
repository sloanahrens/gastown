package daemon

import (
	"fmt"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// A gc that stops happening is invisible: from 2026-09-25 to 2026-10-12 the
// 03:00 window never opened (the host is off overnight) and the gt database
// went 17 days with no completed gc, with nothing said (gt-2l9zd). This check
// rides the existing scheduled_maintenance tick and raises the daemon's keyed
// escalations — the mechanism the rebuild-gt alerts use — so a database past
// its window reaches the attention queue, the health line's escalation count
// and the operator, and clears through the same key once its gc completes
// (gt-vwry).

const (
	// maintenanceGCStaleAfter is how old a database's last completed gc may be
	// before it is an item: ten days, two missed weekly windows. One missed
	// window is the window being busy; two in a row means gc is not happening.
	maintenanceGCStaleAfter = 10 * 24 * time.Hour

	// maintenanceGCAlertSource labels every alert this check raises.
	maintenanceGCAlertSource = "scheduled_maintenance"

	// maintenanceGCStaleAlertSeverity keeps the town's disk from going
	// unmanaged without paging a person for it: the growth is real but nothing
	// has failed yet.
	maintenanceGCStaleAlertSeverity = config.SeverityMedium

	// doltGCStaleAlertKeyPrefix namespaces one database's alert key.
	doltGCStaleAlertKeyPrefix = "dolt-gc-stale:"

	// doltGCStateUnreadableAlertKey is the one item an unreadable state file
	// raises: with the file unreadable no database's gc age is known, so
	// silence there would hide every database at once.
	doltGCStateUnreadableAlertKey = "dolt-gc-state-unreadable"
)

// doltGCStaleAlertKey is one database's stale-gc alert key.
func doltGCStaleAlertKey(db string) string { return doltGCStaleAlertKeyPrefix + db }

// checkStaleMaintenanceGC hands the stale-gc check to a goroutine and returns.
// It is called from the daemon's select loop, and each gt escalate it makes can
// take about three minutes when Dolt hangs (60s x 3 attempts plus backoff), so
// run inline it would hold the Dolt health check, the heartbeat and signal
// handling for that long per stale database (gt-3v0sf).
//
// One check is in flight at a time: a tick that finds the previous one still
// delivering returns, and the next tick starts again from the state file, so a
// slow Dolt neither stacks goroutines nor raises one alert twice at once.
func (d *Daemon) checkStaleMaintenanceGC(now time.Time) {
	if !d.staleGCCheckRunning.CompareAndSwap(false, true) {
		d.logger.Printf("scheduled_maintenance: stale-gc check still delivering from an earlier tick; skipping this one")
		return
	}
	d.maintenance().dispatch(func() {
		defer d.staleGCCheckRunning.Store(false)
		d.settleStaleMaintenanceGC(now)
	})
}

// settleStaleMaintenanceGC raises one alert per database whose last completed gc
// is older than maintenanceGCStaleAfter and clears the ones that have had a gc
// since. It runs on every scheduled_maintenance tick, in the window and out of
// it: the failure it exists for is the window never opening at all, which an
// in-window check would sleep through.
//
// Which keys are open is read from, and written back to, the state file: this
// tick fires every five minutes while gt escalate records a repeat on the open
// escalation bead — a Dolt write per call — so only the transition into and out
// of staleness costs one, and a daemon that restarted in between still clears
// the key whose database recovered while it was down (gt-g49yk).
func (d *Daemon) settleStaleMaintenanceGC(now time.Time) {
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		// Fail closed. An unreadable file does not say every database is fine,
		// it says none of them can be judged, so the one item naming the file
		// is raised in place of a silent pass over all of them.
		d.raiseStaleGCStateUnreadable(fmt.Sprintf(
			"cannot read %s, so no database's gc age is known: %v", maintenanceGCStateFileName, err))
		return
	}
	// The file read, so the unreadable item's condition has gone. Whether that
	// item is open is this process's own memory, not the state file's: the file
	// that would record it is the one that could not be read.
	d.clearStaleGCStateUnreadable("the maintenance state file reads again")

	databases, err := d.maintenance().gcDatabases(d.maintenanceDataDir())
	if err != nil || len(databases) == 0 {
		// The same discovery the gc cycle makes, and it logs its own failure.
		// No names means no database to judge and no stale key to clear.
		return
	}
	stale, current := staleMaintenanceGCs(now, databases, st)

	for _, db := range stale {
		if st.StaleGCAlerts[doltGCStaleAlertKey(db)] {
			continue // Open already; raising again is a write per tick (gt-vwry).
		}
		d.raiseStaleGCAlert(doltGCStaleAlertKey(db), staleGCMessage(now, db, st))
	}

	var recovered []string
	discovered := make(map[string]bool, len(databases))
	for _, db := range databases {
		discovered[doltGCStaleAlertKey(db)] = true
	}
	for _, db := range current {
		if key := doltGCStaleAlertKey(db); st.StaleGCAlerts[key] {
			recovered = append(recovered, key)
		}
	}
	// A key whose database is no longer in the data dir is one no gc can ever
	// close, so it goes with the ones that recovered. The order is fixed so a
	// clear is reproducible.
	var dropped []string
	for key := range st.StaleGCAlerts {
		if !discovered[key] {
			dropped = append(dropped, key)
		}
	}
	sort.Strings(dropped)
	d.clearStaleGCAlerts("their last gc is inside the stale window, or the database is gone",
		append(recovered, dropped...)...)
}

// staleMaintenanceGCs splits databases into the ones whose gc age has passed
// maintenanceGCStaleAfter and the ones that are current.
//
// A database with no completed gc of its own borrows the town's newest one as
// its epoch, so a database added since — or one no cycle reaches — is judged by
// whether gc is happening here at all, rather than by an age it does not have.
// A town with no recorded gc anywhere has no history to judge against and
// stays silent.
func staleMaintenanceGCs(now time.Time, databases []string, st maintenanceGCState) (stale, current []string) {
	newest := newestMaintenanceGC(st)
	for _, db := range databases {
		epoch := st.LastGC[db]
		if epoch.IsZero() {
			epoch = newest
		}
		if !epoch.IsZero() && now.Sub(epoch) >= maintenanceGCStaleAfter {
			stale = append(stale, db)
			continue
		}
		current = append(current, db)
	}
	return stale, current
}

// newestMaintenanceGC is the town's most recent completed gc: the last evidence
// that gc is happening at all.
func newestMaintenanceGC(st maintenanceGCState) time.Time {
	var newest time.Time
	for _, at := range st.LastGC {
		if at.After(newest) {
			newest = at
		}
	}
	return newest
}

// staleGCMessage states the reading that raised the item, in the escalation's
// one-line title.
func staleGCMessage(now time.Time, db string, st maintenanceGCState) string {
	window := townhealth.Short(maintenanceGCStaleAfter)
	if last := st.LastGC[db]; !last.IsZero() {
		return fmt.Sprintf("%s has had no completed gc for %s (last %s, stale after %s); Dolt disk is not reclaimed until one runs",
			db, townhealth.Short(now.Sub(last)), last.UTC().Format(time.RFC3339), window)
	}
	return fmt.Sprintf("%s has no completed gc recorded, and none has run in the town for %s (stale after %s); Dolt disk is not reclaimed until one runs",
		db, townhealth.Short(now.Sub(newestMaintenanceGC(st))), window)
}

// raiseStaleGCAlert raises one database's keyed alert and records the key open.
// The record follows the delivery, so an alert gt escalate dropped is raised
// again on the next tick rather than remembered as delivered (gt-kox0).
func (d *Daemon) raiseStaleGCAlert(key, message string) {
	if err := d.escalateAlertSeverity(maintenanceGCStaleAlertSeverity, key, maintenanceGCAlertSource, message); err != nil {
		d.logger.Printf("scheduled_maintenance: alert %s did not reach the town; retrying next tick: %v", key, err)
		return
	}
	d.noteStaleGCAlert(key, true)
}

// clearStaleGCAlerts closes the keys whose databases are current again. The
// keys are cleared in one call, and only the ones the caller re-checked: a
// blanket close would shut alerts whose condition nothing looked at.
func (d *Daemon) clearStaleGCAlerts(reason string, keys ...string) {
	if len(keys) == 0 {
		return
	}
	if err := d.clearAlertsErr(reason, keys...); err != nil {
		return // The next tick clears them again.
	}
	for _, key := range keys {
		d.noteStaleGCAlert(key, false)
	}
}

// raiseStaleGCStateUnreadable raises the unreadable-state item once per
// process. A restart re-raises it onto the same bead rather than minting a
// second one, which is the dedupe gt escalate performs for any repeated key.
func (d *Daemon) raiseStaleGCStateUnreadable(message string) {
	if d.gcStateUnreadableRaised.Load() {
		return
	}
	if err := d.escalateAlertSeverity(maintenanceGCStaleAlertSeverity, doltGCStateUnreadableAlertKey, maintenanceGCAlertSource, message); err != nil {
		d.logger.Printf("scheduled_maintenance: alert %s did not reach the town; retrying next tick: %v", doltGCStateUnreadableAlertKey, err)
		return
	}
	d.gcStateUnreadableRaised.Store(true)
}

// clearStaleGCStateUnreadable closes the unreadable-state item, if this process
// raised it.
func (d *Daemon) clearStaleGCStateUnreadable(reason string) {
	if !d.gcStateUnreadableRaised.Load() {
		return
	}
	if err := d.clearAlertsErr(reason, doltGCStateUnreadableAlertKey); err != nil {
		return
	}
	d.gcStateUnreadableRaised.Store(false)
}

// noteStaleGCAlert records key as open or closed in the state file. A write
// that fails costs at most one repeat or one redundant clear on the next tick.
func (d *Daemon) noteStaleGCAlert(key string, open bool) {
	if _, err := updateMaintenanceGCState(d.config.TownRoot, func(st *maintenanceGCState) {
		if open {
			st.StaleGCAlerts[key] = true
			return
		}
		delete(st.StaleGCAlerts, key)
	}); err != nil {
		d.logger.Printf("scheduled_maintenance: cannot record alert %s as open=%v: %v", key, open, err)
	}
}
