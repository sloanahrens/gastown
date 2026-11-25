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
