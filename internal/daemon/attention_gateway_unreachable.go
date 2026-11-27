package daemon

import (
	"context"
	"sync"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// gatewayUnreachableCollector raises bd_gateway.unreachable on the counter's growth past the bar.
type gatewayUnreachableCollector struct {
	stats func(ctx context.Context) (bdgateway.GatewayStats, error)
	mu    sync.Mutex
	last  *uint64
}

// collect returns the rule's item for the growth; a failed read is returned and keeps the base.
func (c *gatewayUnreachableCollector) collect(ctx context.Context) ([]attention.Item, error) {
	stats, err := c.stats(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	prev := c.last
	if stats.Unreachable != nil {
		cur := *stats.Unreachable
		c.last = &cur
	}
	c.mu.Unlock()
	return attention.GatewayUnreachableItems(prev, stats.Unreachable), nil
}

func (s *attentionSources) collectGatewayUnreachable(ctx context.Context) ([]attention.Item, error) {
	if s.gatewayUnreachable == nil {
		return nil, nil
	}
	return s.gatewayUnreachable.collect(ctx)
}
