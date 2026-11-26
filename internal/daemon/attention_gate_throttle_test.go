package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// TestAttentionGateThrottleOwnedPredicate: the queue's collector set has a
// gate-throttle-unread entry whose owns predicate keeps only that kind under
// its key prefix, so a failed read keeps its own items and no other kind's.
func TestAttentionGateThrottleOwnedPredicate(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	c := collectorFor(t, f.src, attention.KindGateThrottleUnread)
	if c.owns == nil {
		t.Fatal("the gate-throttle-unread collector has no owns predicate")
	}
	cases := []struct {
		name string
		it   attention.Item
		want bool
	}{
		{"a gate-throttle-unread item is owned", attention.Item{Kind: attention.KindGateThrottleUnread, Key: "gate-throttle-unread:gastown"}, true},
		{"another kind under the gate-throttle key is not", attention.Item{Kind: attention.KindGateSlow, Key: "gate-throttle-unread:gastown"}, false},
		{"a gate-throttle-unread kind under another key is not", attention.Item{Kind: attention.KindGateThrottleUnread, Key: "gate-slow:gastown"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.owns(tc.it); got != tc.want {
				t.Errorf("owns(%+v) = %v, want %v", tc.it, got, tc.want)
			}
		})
	}
}

// TestAttentionGateThrottleCollectorRoutesItsItems: the item the registered
// collector raises carries the gate-throttle-unread kind, so it resolves to
// gate.throttle and is not left as an info line with no rule.
func TestAttentionGateThrottleCollectorRoutesItsItems(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	for i := 0; i < gateThrottleWindow; i++ {
		if err := land.AppendGateRun(f.d.config.TownRoot, gateThrottleRun("gastown", -1)); err != nil {
			t.Fatalf("appending a gate run: %v", err)
		}
	}
	c := collectorFor(t, f.src, attention.KindGateThrottleUnread)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("collect returned %d items, want 1: %+v", len(items), items)
	}
	if rule := attention.RuleFor(items[0]); rule != attention.RuleGateThrottle {
		t.Errorf("RuleFor(collected item) = %q, want %q", rule, attention.RuleGateThrottle)
	}
}
