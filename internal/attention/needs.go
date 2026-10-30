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

// OrderByLandingRisk returns the queue ordered as OrderByNeedsHuman orders it,
// with its risk-path items reordered among the places that order gave them by
// the landing battery's risk answer, highest first.
//
// The landing risk ranks the risk-path items against each other; the needs_human
// answer still says where those places sit, so every other kind keeps today's
// order and one landing's risk never lifts it above another kind of condition.
// A risk-path item the battery has not answered sorts after every answered one,
// and an answered tie keeps today's order (needs_human highest first, then
// oldest first_seen, then key): the answer ranks the items and removes nothing,
// so the same items come out as go in. With no answers at all the queue is
// exactly OrderByNeedsHuman's. The input slice is left as it is.
func OrderByLandingRisk(items []Item, needs Scores, risks LandingRisks) []Item {
	out := OrderByNeedsHuman(items, needs)
	var slots []int
	var risky []Item
	for i, it := range out {
		if it.Kind == KindRiskPath {
			slots = append(slots, i)
			risky = append(risky, it)
		}
	}
	sort.SliceStable(risky, func(i, j int) bool {
		a, aScored := risks[risky[i].Key]
		b, bScored := risks[risky[j].Key]
		if aScored != bScored {
			return aScored
		}
		return aScored && a > b
	})
	for k, i := range slots {
		out[i] = risky[k]
	}
	return out
}
