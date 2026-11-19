package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// TestAttentionCollectorsIncludeGateSlow: the queue's collector set has a
// gate-slow entry whose owns predicate keeps only gate-slow items, so a failed
// read keeps its own items and no other kind's.
func TestAttentionCollectorsIncludeGateSlow(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	c := collectorFor(t, f.src, attention.KindGateSlow)
	if c.owns == nil {
		t.Fatal("the gate-slow collector has no owns predicate")
	}
	cases := []struct {
		name string
		it   attention.Item
		want bool
	}{
		{"a gate-slow item is owned", attention.Item{Kind: attention.KindGateSlow, Key: "gate-slow:gastown"}, true},
		{"another kind under the gate-slow key is not", attention.Item{Kind: attention.KindSandboxSeatKept, Key: "gate-slow:gastown"}, false},
		{"a gate-slow kind under another key is not", attention.Item{Kind: attention.KindGateSlow, Key: "stall:gastown/opal"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.owns(tc.it); got != tc.want {
				t.Errorf("owns(%+v) = %v, want %v", tc.it, got, tc.want)
			}
		})
	}
}

// TestAttentionGateSlowCollectorRoutesItsItems: the item the registered
// collector raises carries the gate-slow kind, so it resolves to gate.slow and
// is not left as an info line with no rule.
func TestAttentionGateSlowCollectorRoutesItsItems(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	for range gateSlowWindow {
		if err := land.AppendGateRun(f.d.config.TownRoot, gateSlowRun("gastown", true, 400, 120)); err != nil {
			t.Fatalf("appending a gate run: %v", err)
		}
	}
	c := collectorFor(t, f.src, attention.KindGateSlow)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("collect returned %d items, want 1: %+v", len(items), items)
	}
	if rule := attention.RuleFor(items[0]); rule != attention.RuleGateSlow {
		t.Errorf("RuleFor(collected item) = %q, want %q", rule, attention.RuleGateSlow)
	}
}
