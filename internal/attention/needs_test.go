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

func keysOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}
