package attention

import (
	"sort"
	"time"
)

// Result is one reconcile's whole output: the new state, the acks that
// survived, and the transitions to append to events.jsonl. Acks is only as
// fresh as the acks this call was handed, so a writer that shares acks.json
// with another process prunes against State instead (PruneAcks).
type Result struct {
	State  State
	Acks   Acks
	Events []Event
}

// Reconcile folds the items the collectors observed now into the previous
// state. It is pure: the clock and the acks are arguments, and nothing is
// read or written.
//
// An observed item keeps the first_seen it already had and gets last_seen
// now; a new one gets first_seen and last_seen now and a new transition. An
// item that was in the previous state but is not observed is dropped with a
// cleared transition. An observed item whose key is acked is marked acked and
// stays in the state. An ack whose item cleared is dropped, so a recurrence
// raises a new item rather than hiding behind the old ack.
//
// The same observed set reconciled twice yields zero transitions.
func Reconcile(prev State, observed []Item, acks Acks, now time.Time) Result {
	prevByKey := make(map[string]Item, len(prev.Items))
	for _, it := range prev.Items {
		prevByKey[it.Key] = it
	}
	ackByKey := make(map[string]time.Time, len(acks.Acks))
	for _, a := range acks.Acks {
		ackByKey[a.Key] = a.At
	}

	items := make([]Item, 0, len(observed))
	seen := make(map[string]bool, len(observed))
	var events []Event
	for _, o := range observed {
		if o.Key == "" || seen[o.Key] {
			continue
		}
		seen[o.Key] = true
		it := o
		if p, ok := prevByKey[o.Key]; ok {
			it.FirstSeen = p.FirstSeen
		} else {
			it.FirstSeen = now
			events = append(events, transition(it, EventNew, now))
		}
		it.LastSeen = now
		if at, ok := ackByKey[o.Key]; ok {
			ackedAt := at
			it.AckedAt = &ackedAt
		}
		items = append(items, it)
	}

	for _, p := range prev.Items {
		if !seen[p.Key] {
			events = append(events, transition(p, EventCleared, now))
		}
	}

	kept := Acks{Acks: make([]Ack, 0, len(acks.Acks))}
	for _, a := range acks.Acks {
		if seen[a.Key] {
			kept.Acks = append(kept.Acks, a)
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	sort.SliceStable(kept.Acks, func(i, j int) bool { return kept.Acks[i].Key < kept.Acks[j].Key })

	return Result{
		State:  State{Updated: now, Items: items},
		Acks:   kept,
		Events: events,
	}
}

// transition renders one item's transition to an events.jsonl line.
func transition(it Item, state EventState, now time.Time) Event {
	return Event{
		TS:       now,
		Class:    it.Kind,
		Severity: it.Severity,
		Text:     it.Summary,
		Key:      it.Key,
		State:    state,
	}
}
