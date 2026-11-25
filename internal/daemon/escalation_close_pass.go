package daemon

import "sync/atomic"

// runEscalationClosePass runs the standalone escalation-close pass when the
// patrol_scan patrol is active and the patrol_scan flag is free. It returns
// true when run ran. A held flag is the patrol_scan tick's single-flight, and
// the tick runs the same pass at its end, so a skip loses nothing (gt-dge4j9).
func runEscalationClosePass(flag *atomic.Bool, active func() bool, run func(), logf func(string, ...any)) bool {
	if !active() {
		return false
	}
	if !runUnderPatrolScanFlag(flag, run) {
		logf("escalation_close: skipped, a patrol_scan tick is running")
		return false
	}
	return true
}

// triggerEscalationClosePass runs one escalation-close pass on its own
// goroutine, so a pass that resumes seats cannot hold the daemon's loop
// (gt-tfbail). The call binds the live daemon's own flag, patrol check, run
// closure and logger, so no unit test drives it: a live daemon shows
// "escalation_close:" lines in its log between patrol_scan ticks.
func (d *Daemon) triggerEscalationClosePass() {
	go func() {
		runEscalationClosePass(&d.patrolScanRunning,
			func() bool { return d.isPatrolActive("patrol_scan") },
			d.escalationCloseRun(),
			d.logger.Printf)
	}()
}

// escalationCloseRun returns a closure that runs one escalation-close nudge
// pass over every rig the patrol_scan patrol covers, through the production
// host runPatrolScanOn builds (gt-qa6c7w).
func (d *Daemon) escalationCloseRun() func() {
	return func() {
		env := &patrolScanHost{d: d}
		d.nudgeClosedEscalations(env, d.getPatrolRigs("patrol_scan"))
	}
}
