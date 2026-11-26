package daemon

import "time"

// openEscalationCloseTicker starts the escalation-close ticker and logs the
// interval it runs at, so a seat answered without a session resumes inside a
// minute rather than at the next patrol_scan pass (gt-3nygtn). It returns the
// tick channel and the stop func for the run loop to keep; the daemon.go bead
// adds that caller, so this bead has none by design.
func (d *Daemon) openEscalationCloseTicker() (<-chan time.Time, func()) {
	ticks, stop := startEscalationCloseTicker(escalationCloseTickInterval)
	d.logger.Printf("Escalation close ticker started (interval %v)", escalationCloseTickInterval)
	return ticks, stop
}
