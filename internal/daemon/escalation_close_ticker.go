package daemon

import "time"

// escalationCloseTickInterval is how often a seat answered without a session is
// looked at, so it resumes inside a minute rather than at the next ten-minute
// patrol_scan pass (gt-k10oes).
const escalationCloseTickInterval = time.Minute

// startEscalationCloseTicker starts the escalation-close ticker on the real
// clock (gt-k10oes).
func startEscalationCloseTicker(interval time.Duration) (<-chan time.Time, func()) {
	return startEscalationCloseTickerWith(interval, func(d time.Duration) (<-chan time.Time, func()) {
		ticker := time.NewTicker(d)
		return ticker.C, ticker.Stop
	})
}

// startEscalationCloseTickerWith starts the escalation-close ticker through
// newTicker, which returns the tick channel and the func that stops it. A seat
// held on an answered escalation resumes within one interval rather than at
// the next patrol_scan pass, ten minutes later (gt-w1uqwu). The seam lets a
// test drive a channel it owns; the real ticker comes from the wrapper bead
// that follows this one.
func startEscalationCloseTickerWith(interval time.Duration, newTicker func(time.Duration) (<-chan time.Time, func())) (<-chan time.Time, func()) {
	return newTicker(interval)
}
