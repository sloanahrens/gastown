package daemon

import "time"

// startEscalationCloseTickerWith starts the escalation-close ticker through
// newTicker, which returns the tick channel and the func that stops it. A seat
// held on an answered escalation resumes within one interval rather than at
// the next patrol_scan pass, ten minutes later (gt-w1uqwu). The seam lets a
// test drive a channel it owns; the real ticker comes from the wrapper bead
// that follows this one.
func startEscalationCloseTickerWith(interval time.Duration, newTicker func(time.Duration) (<-chan time.Time, func())) (<-chan time.Time, func()) {
	return newTicker(interval)
}
