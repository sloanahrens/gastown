package daemon

import (
	"time"
)

// reportPatrolScanDue logs a patrol_scan due decision and reports whether the
// tick should run. A decision carrying a warning logs the WARNING line whether
// or not it is due, so a corrupt last-run record is never hidden behind a skip
// (gt-q0o40s).
func reportPatrolScanDue(dec patrolDueDecision, logf func(string, ...any)) bool {
	switch {
	case dec.warn != "":
		logf("patrol_scan: WARNING: %s — %s", dec.warn, dec.note)
	case dec.due:
		logf("patrol_scan: due — %s", dec.note)
	default:
		logf("patrol_scan: not due — %s", dec.note)
	}
	return dec.due
}

// patrolScanDue decides whether the patrol_scan tick is due, from the persisted
// last run in daemon/patrol_last_run.json, so a daemon restart cannot push the
// next pass back by a full interval (gt-lc0ey5, gt-ima2).
func (d *Daemon) patrolScanDue(now time.Time) patrolDueDecision {
	return evaluatePatrolDue(d.config.TownRoot, "patrol_scan", d.patrolScanRun.last(), now, patrolScanInterval(d.patrol()))
}
