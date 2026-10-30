package attention

import (
	"testing"
	"time"
)

// needsBase is the raise time of the first fixture item; each later item is
// raised a minute after the one before it.
var needsBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func needsItem(key string, raisedMin int) Item {
	return Item{Key: key, Kind: KindRedMain, FirstSeen: needsBase.Add(time.Duration(raisedMin) * time.Minute)}
}

// score is the needs_human answer for one fixture item, keyed the way the Jev
// log names it.
func score(scores Scores, key string, raisedMin int, value float64) Scores {
	if scores == nil {
		scores = Scores{}
	}
	scores[LifeKey(key, needsBase.Add(time.Duration(raisedMin)*time.Minute))] = value
	return scores
}

// The queue is ordered by the needs_human answer, highest first, and an item at
// the same score keeps the order it arrived in. An item no answer names sorts
// after every scored one, still in arrival order, so nothing leaves the queue:
// the score orders it, the precision is too low to clear on it.
func TestOrderByNeedsHuman(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		items  []Item
		scores Scores
		want   []string
	}{
		{
			name:   "the highest score leads, whatever the arrival",
			items:  []Item{needsItem("a", 0), needsItem("b", 1), needsItem("c", 2)},
			scores: score(score(score(nil, "a", 0, 0.2), "b", 1, 0.9), "c", 2, 0.5),
			want:   []string{"b", "c", "a"},
		},
		{
			name:   "a tie keeps arrival order",
			items:  []Item{needsItem("c", 2), needsItem("a", 0), needsItem("b", 1)},
			scores: score(score(score(nil, "a", 0, 0.5), "b", 1, 0.5), "c", 2, 0.9),
			want:   []string{"c", "a", "b"},
		},
		{
			name:   "an item with no answer sorts after every scored one",
			items:  []Item{needsItem("a", 0), needsItem("b", 1), needsItem("c", 2)},
			scores: score(nil, "b", 1, 0.1),
			want:   []string{"b", "a", "c"},
		},
		{
			name:  "with no answers at all the queue keeps arrival order",
			items: []Item{needsItem("b", 1), needsItem("a", 0), needsItem("c", 2)},
			want:  []string{"a", "b", "c"},
		},
		{
			name:   "a low score still keeps the item",
			items:  []Item{needsItem("a", 0), needsItem("b", 1), needsItem("c", 2)},
			scores: score(score(score(nil, "a", 0, 0.01), "b", 1, 0.99), "c", 2, 0.5),
			want:   []string{"b", "c", "a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := OrderByNeedsHuman(tc.items, tc.scores)
			if len(got) != len(tc.items) {
				t.Fatalf("the order holds %d items, want all %d: a score clears nothing", len(got), len(tc.items))
			}
			for i, want := range tc.want {
				if got[i].Key != want {
					t.Fatalf("item %d is %q, want %q (order %v)", i, got[i].Key, want, keysOf(got))
				}
			}
		})
	}
}

// A score belongs to one life of an item, not to its key: a recurrence is a new
// life and must not inherit the answer given to the one before it.
func TestLifeKeySeparatesALifeFromItsRecurrence(t *testing.T) {
	t.Parallel()

	first := LifeKey("red-main:gastown", needsBase)
	if got := LifeKey("red-main:gastown", needsBase.In(time.FixedZone("elsewhere", 5*60*60))); got != first {
		t.Errorf("the same instant in another zone names %q, want %q", got, first)
	}
	if again := LifeKey("red-main:gastown", needsBase.Add(time.Second)); again == first {
		t.Errorf("a recurrence a second later reuses the life key %q", first)
	}
	if other := LifeKey("red-main:other", needsBase); other == first {
		t.Errorf("another item at the same instant reuses the life key %q", first)
	}
}

// The input slice is the queue as read; ordering it for display must not
// reorder the caller's copy.
func TestOrderByNeedsHumanLeavesTheInputOrder(t *testing.T) {
	t.Parallel()

	items := []Item{needsItem("a", 0), needsItem("b", 1)}
	scores := score(nil, "b", 1, 0.9)
	_ = OrderByNeedsHuman(items, scores)
	if items[0].Key != "a" || items[1].Key != "b" {
		t.Errorf("the input order became %v", keysOf(items))
	}
}

// riskPathItem is a landing review raised at raisedMin, the kind the risk-path
// collector raises for a landing that touched the risk list.
func riskPathItem(key string, raisedMin int) Item {
	return Item{Key: key, Kind: KindRiskPath, FirstSeen: needsBase.Add(time.Duration(raisedMin) * time.Minute)}
}

// The landing risk ranks the risk-path items against each other, in the places
// the needs_human order gave them: every other kind keeps today's order, and the
// risk moves nothing off the queue.
func TestOrderByLandingRisk(t *testing.T) {
	t.Parallel()

	const (
		riskA = "risk:gt-a:aaaaaaaaaaaa"
		riskB = "risk:gt-b:bbbbbbbbbbbb"
		riskC = "risk:gt-c:cccccccccccc"
	)
	items := []Item{
		riskPathItem(riskA, 0),
		needsItem("red-main:gastown", 1),
		riskPathItem(riskB, 2),
		riskPathItem(riskC, 3),
		needsItem("stall:gastown/opal", 4),
	}
	needs := score(nil, "red-main:gastown", 1, 0.95)
	for _, at := range []struct {
		key string
		min int
	}{{riskA, 0}, {riskB, 2}, {riskC, 3}} {
		needs = score(needs, at.key, at.min, 0.90)
	}
	needs = score(needs, "stall:gastown/opal", 4, 0.50)
	risks := LandingRisks{riskA: 1, riskB: 3, riskC: 2}

	got := OrderByLandingRisk(items, needs, risks)
	if len(got) != len(items) {
		t.Fatalf("the order holds %d items, want all %d: the risk clears nothing", len(got), len(items))
	}
	if !sameItems(items, got) {
		t.Fatalf("the order holds %v, want the same items as %v", keysOf(got), keysOf(items))
	}
	// The risk-path items lead by their landing risk, highest first, in the
	// places today's order gave the kind.
	wantRisky := []string{riskB, riskC, riskA}
	if risky := riskPathKeys(got); !sameKeys(risky, wantRisky) {
		t.Errorf("the risk-path items are %v, want the landing risk order %v", risky, wantRisky)
	}
	// Every other kind keeps the order needs_human alone gives it.
	wantOther := otherKindKeys(OrderByNeedsHuman(items, needs))
	if other := otherKindKeys(got); !sameKeys(other, wantOther) {
		t.Errorf("the other kinds are %v, want today's order %v", other, wantOther)
	}
	want := []string{"red-main:gastown", riskB, riskC, riskA, "stall:gastown/opal"}
	if !sameKeys(keysOf(got), want) {
		t.Errorf("the queue is %v, want %v", keysOf(got), want)
	}
}

// An item no landing answer names sorts after every answered risk-path item,
// and an answered tie keeps today's order: needs_human highest first, then the
// oldest first_seen, then the key.
func TestOrderByLandingRiskUnansweredAndTies(t *testing.T) {
	t.Parallel()

	const (
		riskX  = "risk:gt-x:xxxxxxxxxxxx"
		riskY  = "risk:gt-y:yyyyyyyyyyyy"
		riskZ  = "risk:gt-z:zzzzzzzzzzzz"
		riskP  = "risk:gt-p:pppppppppppp"
		riskQ  = "risk:gt-q:qqqqqqqqqqqq"
		riskS  = "risk:gt-s:ssssssssssss"
		riskK1 = "risk:gt-k1:111111111111"
		riskK2 = "risk:gt-k2:222222222222"
	)
	tests := []struct {
		name  string
		items []Item
		needs Scores
		risks LandingRisks
		want  []string
	}{
		{
			name:  "an unanswered item sorts after every answered one",
			items: []Item{riskPathItem(riskX, 0), riskPathItem(riskY, 1), riskPathItem(riskZ, 2)},
			needs: score(score(score(nil, riskX, 0, 0.5), riskY, 1, 0.5), riskZ, 2, 0.5),
			risks: LandingRisks{riskY: 1, riskZ: 2},
			want:  []string{riskZ, riskY, riskX},
		},
		{
			name:  "a risk of zero is an answer and still leads an unanswered item",
			items: []Item{riskPathItem(riskX, 0), riskPathItem(riskY, 1)},
			needs: score(score(nil, riskX, 0, 0.5), riskY, 1, 0.5),
			risks: LandingRisks{riskY: 0},
			want:  []string{riskY, riskX},
		},
		{
			name:  "a tie falls back to needs_human and then the oldest first_seen",
			items: []Item{riskPathItem(riskP, 0), riskPathItem(riskQ, 5), riskPathItem(riskS, 2)},
			needs: score(score(score(nil, riskP, 0, 0.30), riskQ, 5, 0.70), riskS, 2, 0.70),
			risks: LandingRisks{riskP: 2, riskQ: 2, riskS: 2},
			want:  []string{riskS, riskQ, riskP},
		},
		{
			name:  "a tie at one answer and one raise falls back to the key",
			items: []Item{riskPathItem(riskK2, 0), riskPathItem(riskK1, 0)},
			needs: score(score(nil, riskK1, 0, 0.5), riskK2, 0, 0.5),
			risks: LandingRisks{riskK1: 2, riskK2: 2},
			want:  []string{riskK1, riskK2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := keysOf(OrderByLandingRisk(tc.items, tc.needs, tc.risks)); !sameKeys(got, tc.want) {
				t.Errorf("the queue is %v, want %v", got, tc.want)
			}
		})
	}
}

// sameItems reports whether two orders hold the same items, neither dropping nor
// duplicating one.
func sameItems(a, b []Item) bool {
	counts := map[string]int{}
	for _, it := range b {
		counts[it.Key]++
	}
	for _, it := range a {
		counts[it.Key]--
		if counts[it.Key] < 0 {
			return false
		}
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// sameKeys reports whether two key lists name the same keys in the same order.
func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// riskPathKeys is the order's risk-path items, in order.
func riskPathKeys(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Kind == KindRiskPath {
			out = append(out, it.Key)
		}
	}
	return out
}

// otherKindKeys is the order's items of every kind but risk-path, in order.
func otherKindKeys(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Kind != KindRiskPath {
			out = append(out, it.Key)
		}
	}
	return out
}

func keysOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}
