package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// capTick is one tick as the fake Stats source answers it: the read's counters,
// or the error the read returned.
type capTick struct {
	stats bdgateway.GatewayStats
	err   error
	// wantItems is how many items the tick raises; wantSummary is the summary of
	// the one item, when it raises one.
	wantItems   int
	wantSummary string
	wantErr     bool
}

// TestCollectBDGatewayCapRaises drives the cap collector tick by tick over a
// fake Stats source. The first read only sets the base; a TooManyCalls rise
// since the previous good read raises one item naming the refused count and the
// peak; an equal read raises nothing; a failed read is returned and takes no
// sample, so the next good read still measures from the last good base.
func TestCollectBDGatewayCapRaises(t *testing.T) {
	t.Parallel()
	const raised = "bd gateway refused 3 calls at its concurrency cap (peak in flight 5)"
	cases := []struct {
		name  string
		ticks []capTick
	}{
		{
			name: "growth from 0 to 3 raises one item naming 3 refused",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 0, PeakInFlight: 2}},
				{stats: bdgateway.GatewayStats{TooManyCalls: 3, PeakInFlight: 5}, wantItems: 1, wantSummary: raised},
			},
		},
		{
			name: "an equal second read raises nothing",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 0, PeakInFlight: 2}},
				{stats: bdgateway.GatewayStats{TooManyCalls: 3, PeakInFlight: 5}, wantItems: 1, wantSummary: raised},
				{stats: bdgateway.GatewayStats{TooManyCalls: 3, PeakInFlight: 5}},
			},
		},
		{
			name: "the first read only sets the base",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 7, PeakInFlight: 4}},
			},
		},
		{
			name: "a failed read is returned and the next read measures from the last good base",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 0, PeakInFlight: 2}},
				{err: errors.New("gateway down"), wantErr: true},
				{stats: bdgateway.GatewayStats{TooManyCalls: 3, PeakInFlight: 5}, wantItems: 1, wantSummary: raised},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cur capTick
			c := &gatewayCapCollector{stats: func(context.Context) (bdgateway.GatewayStats, error) {
				return cur.stats, cur.err
			}}
			for i, tk := range tc.ticks {
				cur = tk
				items, err := c.collect(context.Background())
				if (err != nil) != tk.wantErr {
					t.Fatalf("tick %d: err = %v, want error %v", i, err, tk.wantErr)
				}
				if len(items) != tk.wantItems {
					t.Fatalf("tick %d: raised %d items, want %d", i, len(items), tk.wantItems)
				}
				if tk.wantItems == 0 {
					continue
				}
				item := items[0]
				if item.Kind != attention.KindBDGatewayCapHit {
					t.Errorf("tick %d: kind = %s, want %s", i, item.Kind, attention.KindBDGatewayCapHit)
				}
				if item.Summary != tk.wantSummary {
					t.Errorf("tick %d: summary = %q, want %q", i, item.Summary, tk.wantSummary)
				}
			}
		})
	}
}

// TestCollectBDGatewayCapNoSource: a collector with no Stats source raises
// nothing and reads no error, the same as a daemon with no gateway configured.
func TestCollectBDGatewayCapNoSource(t *testing.T) {
	t.Parallel()
	var c gatewayCapCollector
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect with no source: err = %v, want nil", err)
	}
	if len(items) != 0 {
		t.Fatalf("collect with no source raised %d items, want none", len(items))
	}
}
