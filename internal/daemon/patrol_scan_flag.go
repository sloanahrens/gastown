package daemon

import "sync/atomic"

// runUnderPatrolScanFlag runs run while holding flag, the single-flight
// exclusion the standalone escalation-close pass shares with the patrol_scan
// tick. It returns false without running when the flag is already held. The
// flag is released on every path, including a panic in run, which propagates.
func runUnderPatrolScanFlag(flag *atomic.Bool, run func()) bool {
	if !flag.CompareAndSwap(false, true) {
		return false
	}
	defer flag.Store(false)
	run()
	return true
}
