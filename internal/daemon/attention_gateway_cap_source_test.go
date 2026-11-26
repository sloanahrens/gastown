package daemon

import (
	"context"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// TestAttentionSourcesGatewayCapNilField: with no cap collector wired, the
// source raises nothing and reads no error, the same as a daemon with no
// gateway configured.
func TestAttentionSourcesGatewayCapNilField(t *testing.T) {
	t.Parallel()
	s := &attentionSources{}
	items, err := s.collectBDGatewayCap(context.Background())
	if err != nil {
		t.Fatalf("collectBDGatewayCap with nil field: err = %v, want nil", err)
	}
	if len(items) != 0 {
		t.Fatalf("collectBDGatewayCap with nil field raised %d items, want none", len(items))
	}
}

// TestAttentionSourcesGatewayCapRaises: the source runs the collector it holds.
// The first tick only sets the collector's base; a refusal count that grows
// before the second tick raises the cap item.
func TestAttentionSourcesGatewayCapRaises(t *testing.T) {
	t.Parallel()
	reads := []bdgateway.GatewayStats{
		{TooManyCalls: 0, PeakInFlight: 2},
		{TooManyCalls: 3, PeakInFlight: 5},
	}
	var tick int
	c := &gatewayCapCollector{stats: func(context.Context) (bdgateway.GatewayStats, error) {
		st := reads[tick]
		tick++
		return st, nil
	}}
	s := &attentionSources{gatewayCap: c}
	ctx := context.Background()

	first, err := s.collectBDGatewayCap(ctx)
	if err != nil {
		t.Fatalf("first tick: err = %v, want nil", err)
	}
	if len(first) != 0 {
		t.Fatalf("first tick raised %d items, want none (it only sets the base)", len(first))
	}

	second, err := s.collectBDGatewayCap(ctx)
	if err != nil {
		t.Fatalf("second tick: err = %v, want nil", err)
	}
	if len(second) != 1 {
		t.Fatalf("second tick raised %d items, want 1", len(second))
	}
	if second[0].Kind != attention.KindBDGatewayCapHit {
		t.Errorf("second tick kind = %s, want %s", second[0].Kind, attention.KindBDGatewayCapHit)
	}
}
