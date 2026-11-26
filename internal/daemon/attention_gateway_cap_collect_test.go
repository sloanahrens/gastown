package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// capTick is one tick as the fake Stats source answers it: the read's counters,
// or the error the read returned.
type capTick struct {
	stats bdgateway.GatewayStats
	// absent marks a body that carried no cap fields: the fake reports it with
	// CapsPresent false. A tick without it is a present read.
	absent bool
	err    error
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
			name: "an absent read raises nothing and the next present read measures from the last present base",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 0, PeakInFlight: 2}},
				{stats: bdgateway.GatewayStats{TooManyCalls: 9, PeakInFlight: 0}, absent: true},
				{stats: bdgateway.GatewayStats{TooManyCalls: 3, PeakInFlight: 5}, wantItems: 1, wantSummary: raised},
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
				s := cur.stats
				s.CapsPresent = !cur.absent
				return s, cur.err
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

// TestCollectBDGatewayCapStatsError: a failed Stats read is returned as the
// collector's error, raises no item and takes no sample. The error rows carry
// zero counters, so a collector that sampled the failed read would reset the
// base to zero, and the next good read would raise from there.
func TestCollectBDGatewayCapStatsError(t *testing.T) {
	t.Parallel()
	errDown := errors.New("gateway down")
	const raised = "bd gateway refused 2 calls at its concurrency cap (peak in flight 5)"
	cases := []struct {
		name  string
		ticks []capTick
	}{
		{
			name: "the next good read measures from the base before the failure",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 4, PeakInFlight: 2}},
				{err: errDown},
				{stats: bdgateway.GatewayStats{TooManyCalls: 6, PeakInFlight: 5}, wantItems: 1, wantSummary: raised},
			},
		},
		{
			name: "a run of failed reads keeps the base",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 4, PeakInFlight: 2}},
				{err: errDown},
				{err: errDown},
				{stats: bdgateway.GatewayStats{TooManyCalls: 4, PeakInFlight: 2}},
			},
		},
		{
			name: "a failed read before any good read leaves the first good read as the base",
			ticks: []capTick{
				{err: errDown},
				{stats: bdgateway.GatewayStats{TooManyCalls: 7, PeakInFlight: 3}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cur capTick
			c := &gatewayCapCollector{stats: func(context.Context) (bdgateway.GatewayStats, error) {
				s := cur.stats
				s.CapsPresent = true
				return s, cur.err
			}}
			for i, tk := range tc.ticks {
				cur = tk
				items, err := c.collect(context.Background())
				if !errors.Is(err, tk.err) {
					t.Fatalf("tick %d: err = %v, want %v", i, err, tk.err)
				}
				if len(items) != tk.wantItems {
					t.Fatalf("tick %d: raised %d items, want %d", i, len(items), tk.wantItems)
				}
				if tk.wantItems == 0 {
					continue
				}
				if items[0].Summary != tk.wantSummary {
					t.Errorf("tick %d: summary = %q, want %q", i, items[0].Summary, tk.wantSummary)
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

// TestCollectBDGatewayCapAbsentNoLogger: a collector with no logf still reads an
// absent body as no item and no error; the line is skipped, not a panic.
func TestCollectBDGatewayCapAbsentNoLogger(t *testing.T) {
	t.Parallel()
	c := &gatewayCapCollector{stats: func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{}, nil
	}}
	for i := range 2 {
		items, err := c.collect(context.Background())
		if err != nil {
			t.Fatalf("tick %d: err = %v, want nil", i, err)
		}
		if len(items) != 0 {
			t.Fatalf("tick %d raised %d items, want none", i, len(items))
		}
	}
}

// TestCollectBDGatewayCapAbsent: a stats body with no cap fields raises no item,
// and the collector says once in the run that the gateway does not report its
// cap counters. The first absent read of a run logs; later absent reads do not.
// A present read before the absent ones does not stop the line.
func TestCollectBDGatewayCapAbsent(t *testing.T) {
	t.Parallel()
	const said = "attention: bd gateway does not report its cap counters (/v1/stats has no cap fields); bd_gateway.cap_hit waits for them"
	cases := []struct {
		name      string
		ticks     []capTick
		wantLines int
	}{
		{
			name: "two absent reads raise no item and say it once",
			ticks: []capTick{
				{absent: true},
				{absent: true},
			},
			wantLines: 1,
		},
		{
			name: "a present read before the absent ones still says it once",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 0, PeakInFlight: 2}},
				{absent: true},
				{absent: true},
			},
			wantLines: 1,
		},
		{
			name: "present reads say nothing",
			ticks: []capTick{
				{stats: bdgateway.GatewayStats{TooManyCalls: 0, PeakInFlight: 2}},
				{stats: bdgateway.GatewayStats{TooManyCalls: 3, PeakInFlight: 5}, wantItems: 1},
			},
			wantLines: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cur capTick
			var lines []string
			c := &gatewayCapCollector{
				stats: func(context.Context) (bdgateway.GatewayStats, error) {
					s := cur.stats
					s.CapsPresent = !cur.absent
					return s, cur.err
				},
				logf: func(format string, args ...any) {
					lines = append(lines, fmt.Sprintf(format, args...))
				},
			}
			for i, tk := range tc.ticks {
				cur = tk
				items, err := c.collect(context.Background())
				if err != nil {
					t.Fatalf("tick %d: err = %v, want nil", i, err)
				}
				if len(items) != tk.wantItems {
					t.Fatalf("tick %d: raised %d items, want %d", i, len(items), tk.wantItems)
				}
			}
			if len(lines) != tc.wantLines {
				t.Fatalf("logged %d lines, want %d: %q", len(lines), tc.wantLines, lines)
			}
			for _, line := range lines {
				if line != said {
					t.Errorf("line = %q, want %q", line, said)
				}
			}
		})
	}
}
