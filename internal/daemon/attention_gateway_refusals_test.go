package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// refusalsTick is the time the refusals cases tick at.
var refusalsTick = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

// refusalsTickAt ticks the refusals collector at at, with a gateway whose stats
// read answers counts as its refusals.
func refusalsTickAt(t *testing.T, f *attentionFixture, at time.Time, counts map[string]uint64) []attention.Item {
	t.Helper()
	f.src.now = at
	f.src.gatewayStats = func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{Refusals: counts, RefusalsPresent: true}, nil
	}
	return f.collect(t, f.src.collectGatewayRefusals)
}

// TestGatewayRefusalsAttentionRaises: growth of more than five refused calls in
// the hour raises one item naming each reason and its count, growth of five
// raises none, and once the rise has aged out of the hour a quiet tick clears it.
func TestGatewayRefusalsAttentionRaises(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, refusalsTick)
	f.src.refusalWindow = &RefusalWindow{}
	refusalsTickAt(t, f, refusalsTick, map[string]uint64{"cap": 0, "stale": 0})

	items := refusalsTickAt(t, f, refusalsTick.Add(10*time.Minute), map[string]uint64{"cap": 3, "stale": 2})
	if len(items) != 0 {
		t.Fatalf("growth of 5 raised %d items, want none", len(items))
	}

	items = refusalsTickAt(t, f, refusalsTick.Add(20*time.Minute), map[string]uint64{"cap": 4, "stale": 2})
	if len(items) != 1 {
		t.Fatalf("growth of 6 raised %d items, want one", len(items))
	}
	if items[0].Kind != attention.KindBdGatewayRefusals {
		t.Errorf("kind = %s, want %s", items[0].Kind, attention.KindBdGatewayRefusals)
	}
	for _, want := range []string{"cap 4", "stale 2"} {
		if !strings.Contains(items[0].Summary, want) {
			t.Errorf("summary %q does not name %q", items[0].Summary, want)
		}
	}

	// The rise was recorded at twenty minutes, so at eighty minutes it has aged
	// out of the hour and only the latest sample is left: no growth.
	items = refusalsTickAt(t, f, refusalsTick.Add(time.Hour+20*time.Minute), map[string]uint64{"cap": 4, "stale": 2})
	if len(items) != 0 {
		t.Errorf("a quiet tick after the rise aged out raised %d items, want none", len(items))
	}
}

// TestGatewayRefusalsStatsErrorIsReturned: a failed stats read returns its error,
// so the collector keeps the previous item, and it takes no sample into the window.
func TestGatewayRefusalsStatsErrorIsReturned(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, refusalsTick)
	f.src.refusalWindow = &RefusalWindow{}
	f.src.gatewayStats = func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{}, errors.New("the gateway did not answer")
	}
	items, err := f.src.collectGatewayRefusals(context.Background())
	if err == nil || len(items) != 0 {
		t.Fatalf("a failed stats read returned %d items and error %v, want no items and its error", len(items), err)
	}
	if growth := f.src.refusalWindow.Growth(refusalsTick); len(growth) != 0 {
		t.Errorf("a failed stats read left growth %v in the window, want none", growth)
	}
}
