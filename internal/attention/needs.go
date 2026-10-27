package attention

import (
	"sort"
	"time"
)

// Scores is Jev's needs_human answer per item life: the probability that a
// person has to look at or decide the item, by LifeKey. An item with no entry
// has no answer, which is not the same as a probability of zero.
type Scores map[string]float64

// LifeKey names one life of a queue item: its key and the first_seen that tells
// a recurrence from the life before it.
func LifeKey(key string, firstSeen time.Time) string {
	return key + "@" + firstSeen.UTC().Format(time.RFC3339Nano)
}

// OrderByNeedsHuman returns items ordered by their needs_human score, highest
// first, with items at one score kept in arrival order (oldest first, then
// key).
//
// An item with no score sorts after every scored item, also in arrival order:
// the score ranks the queue, it removes nothing from it, because its precision
// is too low to clear on (docs/reference.md has the figure). Every item is
// returned and a low score only sinks it. The input slice is left as it is.
func OrderByNeedsHuman(items []Item, scores Scores) []Item {
	out := append([]Item(nil), items...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aScored := scores[LifeKey(out[i].Key, out[i].FirstSeen)]
		b, bScored := scores[LifeKey(out[j].Key, out[j].FirstSeen)]
		if aScored != bScored {
			return aScored
		}
		if aScored && a != b {
			return a > b
		}
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].Key < out[j].Key
	})
	return out
}
