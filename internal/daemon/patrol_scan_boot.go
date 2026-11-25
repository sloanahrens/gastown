package daemon

import "sync/atomic"

// runPatrolScanCheck runs one patrol_scan check. The first check after a boot
// scans now, whatever the persisted last run says: a tick that finished less
// than one interval before a shutdown is not due after the restart, so the
// work the shutdown canceled would wait a full interval (gt-3z6wd5). Every
// later check takes the due gate. CompareAndSwap lets exactly one caller win
// the first check when several race.
func runPatrolScanCheck(booted *atomic.Bool, scanNow, scanIfDue func()) {
	if booted.CompareAndSwap(false, true) {
		scanNow()
		return
	}
	scanIfDue()
}
