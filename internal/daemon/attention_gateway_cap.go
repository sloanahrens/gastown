package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// gatewayCapKey is the one item the cap collector raises.
const gatewayCapKey = "bd-gateway-cap-hit"

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

// gatewayCapCollector raises bd_gateway.cap_hit on each tick the gateway's
// refusals at its cap grew since the previous tick. The mutex guards the tracker,
// which is not safe for concurrent use. A nil stats source raises nothing, as a
// daemon with no gateway configured does.
type gatewayCapCollector struct {
	stats   func(ctx context.Context) (bdgateway.GatewayStats, error)
	mu      sync.Mutex
	tracker gatewayCapTracker
}

// collect reads the gateway's stats and feeds the tracker. A failed read is
// returned as an error and takes no sample, so the tracker keeps its last good
// base and the next good read measures growth from there.
func (c *gatewayCapCollector) collect(ctx context.Context) ([]attention.Item, error) {
	if c.stats == nil {
		return nil, nil
	}
	stats, err := c.stats(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	reading := c.tracker.observe(stats)
	c.mu.Unlock()
	if !reading.Raise {
		return nil, nil
	}
	return []attention.Item{{
		Key:      gatewayCapKey,
		Kind:     attention.KindBDGatewayCapHit,
		Severity: attention.SeverityMedium,
		Summary: fmt.Sprintf("bd gateway refused %d calls at its concurrency cap (peak in flight %d)",
			reading.Refused, reading.Peak),
	}}, nil
}
