package daemon

import "time"

// patrolScanCheckTick returns how often the patrol_scan ticker checks whether a
// scan is due: the check cadence shortPatrolCheckTick derives from the
// configured scan interval, so a scan is picked up soon after a daemon restart
// instead of a full interval late (gt-21gba2).
func patrolScanCheckTick(pc *DaemonPatrolConfig) time.Duration {
	return shortPatrolCheckTick(patrolScanInterval(pc))
}
