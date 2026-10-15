package attention

import (
	"testing"
	"time"
)

func TestRiskPathPromotion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	riskItem := func(landedAgo time.Duration) Item {
		it := item("risk:gt-abc:aaaaaaaaaaaa", KindRiskPath, "gt-abc landed aaaaaaaaaaaa touching internal/land/land.go; overseer review wanted")
		it.LandedAt = now.Add(-landedAgo)
		return it
	}
	promoted := func(it Item) Item {
		at := now.Add(-time.Minute)
		it.PromotedAt = &at
		return it
	}
	acked := func(it Item) Item {
		at := now.Add(-time.Hour)
		it.AckedAt = &at
		return it
	}

	tests := []struct {
		name         string
		items        []Item
		wantPromoted bool
	}{
		{name: "just under three hours stays info", items: []Item{riskItem(RiskPathPromoteAfter - time.Second)}},
		{name: "exactly three hours promotes", items: []Item{riskItem(RiskPathPromoteAfter)}, wantPromoted: true},
		{name: "just over three hours promotes", items: []Item{riskItem(RiskPathPromoteAfter + time.Second)}, wantPromoted: true},
		{name: "a long-unreviewed landing promotes", items: []Item{riskItem(30 * time.Hour)}, wantPromoted: true},
		{name: "an item already promoted is not promoted again", items: []Item{promoted(riskItem(4 * time.Hour))}},
		{name: "an acked item is left alone", items: []Item{acked(riskItem(4 * time.Hour))}},
		{name: "an item with no landing time is left alone", items: []Item{item("risk:gt-abc:aaaaaaaaaaaa", KindRiskPath, "no landing time")}},
		{name: "a cleared item is not in the state", items: nil},
		{
			name: "another rule's old item is left alone",
			items: func() []Item {
				it := item("esc:hq-1", KindEscalation, "escalation open")
				it.LandedAt = now.Add(-10 * time.Hour)
				return []Item{it}
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := State{Updated: now, Items: tc.items}
			got, events := RiskPathPromotion(in, now)

			if !tc.wantPromoted {
				if len(events) != 0 {
					t.Fatalf("events = %+v, want none", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events = %+v, want one promotion line", events)
			}
			e := events[0]
			if e.State != EventNew || e.Rule != RuleRiskPath || e.Handled != HandledOperatorAction || !e.Wake || e.Key != tc.items[0].Key {
				t.Errorf("promotion = %+v, want a raised landing.risk_path line, operator_action, wake", e)
			}
			if got.Items[0].PromotedAt == nil || !got.Items[0].PromotedAt.Equal(now) {
				t.Errorf("PromotedAt = %v, want %v", got.Items[0].PromotedAt, now)
			}
			if in.Items[0].PromotedAt != nil {
				t.Error("the input state was modified")
			}
		})
	}
}

// A promoted item is not promoted by the next tick's promotion, and the mark
// survives Reconcile while the item holds; once the item clears and recurs it
// starts unpromoted.
func TestRiskPathPromotionOncePerItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	it := item("risk:gt-abc:aaaaaaaaaaaa", KindRiskPath, "gt-abc landed")
	it.LandedAt = now.Add(-4 * time.Hour)

	res := Reconcile(State{}, []Item{it}, Acks{}, now)
	st, events := RiskPathPromotion(res.State, now)
	if len(events) != 1 {
		t.Fatalf("first tick events = %+v, want the promotion", events)
	}

	later := now.Add(time.Minute)
	res = Reconcile(st, []Item{it}, Acks{}, later)
	st, events = RiskPathPromotion(res.State, later)
	if len(events) != 0 {
		t.Fatalf("second tick events = %+v, want none: the item was promoted", events)
	}

	res = Reconcile(st, nil, Acks{}, later.Add(time.Minute))
	if len(res.State.Items) != 0 {
		t.Fatalf("items = %+v, want the reviewed item cleared", res.State.Items)
	}
	res = Reconcile(res.State, []Item{it}, Acks{}, later.Add(2*time.Minute))
	if _, events = RiskPathPromotion(res.State, later.Add(2*time.Minute)); len(events) != 1 {
		t.Errorf("recurred item events = %+v, want it promoted afresh", events)
	}
}
