package daemon

import (
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

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
