package daemon

import (
	"time"
)

// patrolScanDue decides whether the patrol_scan tick is due, from the persisted
// last run in daemon/patrol_last_run.json, so a daemon restart cannot push the
// next pass back by a full interval (gt-lc0ey5, gt-ima2).
func (d *Daemon) patrolScanDue(now time.Time) patrolDueDecision {
	return evaluatePatrolDue(d.config.TownRoot, "patrol_scan", d.patrolScanRun.last(), now, patrolScanInterval(d.patrol()))
}
