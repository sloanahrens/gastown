package daemon

import "github.com/steveyegge/gastown/internal/bdgateway"

// gatewayCapReading is what one tick's /v1/stats read says about the gateway's
// concurrency cap: Raise when it refused calls at the cap since the previous
// tick, Refused the growth of those refusals, Peak the read's PeakInFlight.
type gatewayCapReading struct {
	Raise   bool
	Refused uint64
	Peak    int
}

// gatewayCapTracker decides, tick by tick, whether the gateway refused calls at
// its cap since the previous tick. The first observation only sets the base.
// State lives in memory for one daemon run. The tracker is not safe for
// concurrent use: its collector owns the lock.
type gatewayCapTracker struct {
	seen bool
	last uint64
}

// observe records one stats read and reports the growth of TooManyCalls since
// the previous read. A counter that did not grow, or dropped, raises nothing.
func (t *gatewayCapTracker) observe(s bdgateway.GatewayStats) gatewayCapReading {
	reading := gatewayCapReading{Peak: s.PeakInFlight}
	if t.seen && s.TooManyCalls > t.last {
		reading.Raise = true
		reading.Refused = s.TooManyCalls - t.last
	}
	t.seen = true
	t.last = s.TooManyCalls
	return reading
}
