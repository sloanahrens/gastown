package daemon

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// reconcilerCycleWanted answers which due duties a maintenance cycle may start
// for now: reconcilerDueDuties' names, and nothing while the patrol is
// shadow, while the town stands by (the ESTOP, a Waned Ring, the boot gate), or
// while a maintenance gc already runs. The town_state duty is judged with
// townStateNightlyRoot's error, so a duty whose root cannot be read is left
// out. It starts nothing (gt-y9xml3).
func (d *Daemon) reconcilerCycleWanted(now time.Time, obs []reconcile.Observation) []string {
	if d.reconcilerShadow() {
		return nil
	}
	if d.standbyReason(now) != "" {
		return nil
	}
	if d.maintenanceGCRunning.Load() {
		return nil
	}
	_, err := d.townStateNightlyRoot()
	return reconcilerDueDuties(obs, err)
}

// reconcilerShadow reports whether the reconciler patrol only judges and logs:
// the Reconciler block's shadow setting, read as runReconciler reads it. No
// block at all reads shadow.
func (d *Daemon) reconcilerShadow() bool {
	if pc := d.patrol(); pc != nil && pc.Patrols != nil {
		return pc.Patrols.Reconciler.ParseShadow()
	}
	return true
}

// reconcilerDueDuties returns, in input order, the duties a maintenance cycle
// can fix and that are due or overdue: backup, archive, a gc of one database,
// and town_state only when its root is readable (townStateRootErr is nil),
// since a town_state duty overdue because its root is unset cannot be fixed by
// a cycle. An ok or unknown verdict, and any other duty name, is left out.
func reconcilerDueDuties(obs []reconcile.Observation, townStateRootErr error) []string {
	var due []string
	for _, o := range obs {
		if o.Verdict != reconcile.VerdictDue && o.Verdict != reconcile.VerdictOverdue {
			continue
		}
		if !reconcilerCycleFixes(o.Duty, townStateRootErr) {
			continue
		}
		due = append(due, o.Duty)
	}
	return due
}

// reconcilerCycleFixes reports whether a maintenance cycle can fix a duty with
// this name: backup, archive, a gc of one database, or town_state when its root
// error is nil. GC observations carry their database after a colon
// (reconcile.GCObservations); a bare gc is a duty-wide unknown the cycle cannot
// act on.
func reconcilerCycleFixes(duty string, townStateRootErr error) bool {
	switch duty {
	case config.ReconcilerDutyBackup, config.ReconcilerDutyArchive:
		return true
	case config.ReconcilerDutyTownState:
		return townStateRootErr == nil
	}
	db, found := strings.CutPrefix(duty, config.ReconcilerDutyGC+":")
	return found && db != ""
}
