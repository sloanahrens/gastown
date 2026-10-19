package attention

import (
	"sort"
	"time"
)

// Record is one item's life as the retained files describe it: from the
// transition that raised it to the one that cleared it, or to now if it still
// stands.
//
// An item raised and cleared twice is two records with the same key, because
// a recurrence is a new item and an ack on the first does not hide the second.
type Record struct {
	// Key is the item's stable identity.
	Key string `json:"key"`
	// Kind is what the item is about.
	Kind Kind `json:"kind"`
	// Severity is how loud the item was.
	Severity Severity `json:"severity"`
	// Rig names the rig the condition was in, when it had one.
	Rig string `json:"rig,omitempty"`
	// Bead is the bead the condition named, when it had one.
	Bead string `json:"bead,omitempty"`
	// SHA is the commit the condition named, when it had one.
	SHA string `json:"sha,omitempty"`
	// Summary is the one-line human description.
	Summary string `json:"summary"`
	// Rule is the stable id of the rule that raised the item, when the raising
	// line carries one.
	Rule string `json:"rule,omitempty"`
	// FirstSeen is when the item was raised: the state's first_seen while it
	// stands, else the new line, else the clear when the raising line is gone.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when the item was last observed: the clear when it has
	// cleared, the daemon's last tick while it stands.
	LastSeen time.Time `json:"last_seen"`
	// ClearedAt is when the item cleared, nil while it stands.
	ClearedAt *time.Time `json:"cleared_at,omitempty"`
	// AckedAt is the ack in force for the item: the state's ack while it
	// stands, or the ack made at or before it cleared.
	AckedAt *time.Time `json:"acked_at,omitempty"`
	// Acked reports whether an ack hid the item: current for an item that
	// stands, and as of its clear for one that has cleared.
	Acked bool `json:"acked"`
	// Transitions are the lines about this item, oldest first.
	Transitions []Event `json:"transitions,omitempty"`
}

// Snapshot is one read of the queue: the files as they stand, the items they
// describe, and the transitions between them.
type Snapshot struct {
	// State is state.json as read; its Updated is zero when the daemon has
	// never written one.
	State State
	// Acks is acks.json as read.
	Acks Acks
	// Items are the item lives the retained files describe, by first_seen.
	Items []Record
	// Transitions are every retained events line, oldest first across both
	// events.jsonl.1 and events.jsonl.
	Transitions []Event
	// Skipped counts the non-blank events lines that did not parse. A process
	// killed mid-append leaves the last one cut short, and losing it beats
	// failing a read that can list the rest.
	Skipped int
}

// Read loads the queue from <town>/.runtime/attention without writing to it,
// so a reader can run beside the daemon and gt attention ack.
func Read(townRoot string) (Snapshot, error) {
	state, err := ReadState(townRoot)
	if err != nil {
		return Snapshot{}, err
	}
	acks, err := ReadAcks(townRoot)
	if err != nil {
		return Snapshot{}, err
	}
	old, oldSkipped, err := readEventsFileCounted(RotatedEventsPath(townRoot))
	if err != nil {
		return Snapshot{}, err
	}
	cur, curSkipped, err := readEventsFileCounted(EventsPath(townRoot))
	if err != nil {
		return Snapshot{}, err
	}
	events := append(old, cur...)
	// Each file is oldest first, but a rotation boundary is not a time
	// boundary: a daemon clock step or a rotation mid-batch can leave the
	// rotated file's tail newer than the current file's head.
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.Before(events[j].TS) })
	return Snapshot{
		State:       state,
		Acks:        acks,
		Items:       records(events, state, acks),
		Transitions: events,
		Skipped:     oldSkipped + curSkipped,
	}, nil
}

// records replays the transitions into one Record per item life and folds the
// standing state in. State is authoritative for what stands now, because an
// item raised before the oldest retained line has no new transition to find.
func records(events []Event, state State, acks Acks) []Record {
	var out []Record
	open := make(map[string]int, len(state.Items))
	cleared := make(map[string]time.Time, len(state.Items))
	for _, ev := range events {
		if ev.State == EventCleared {
			cleared[ev.Key] = ev.TS
			if i, ok := open[ev.Key]; ok {
				at := ev.TS
				r := &out[i]
				r.LastSeen = at
				r.ClearedAt = &at
				r.Transitions = append(r.Transitions, ev)
				delete(open, ev.Key)
				continue
			}
		}
		r := Record{
			Key: ev.Key, Kind: ev.Class, Severity: ev.Severity,
			Summary: ev.Text, Rule: ev.Rule,
			FirstSeen: ev.TS, LastSeen: ev.TS,
			Transitions: []Event{ev},
		}
		if ev.State == EventCleared {
			at := ev.TS
			r.ClearedAt = &at
		} else {
			open[ev.Key] = len(out)
		}
		out = append(out, r)
	}

	for _, it := range state.Items {
		i, ok := open[it.Key]
		if !ok {
			if at, gone := cleared[it.Key]; gone && !it.FirstSeen.After(at) {
				// The state was written before the clear, so it still lists
				// an item the log says is gone. A real recurrence would have
				// a first_seen of its own past the clear.
				continue
			}
			out = append(out, Record{Key: it.Key})
			i = len(out) - 1
		}
		r := &out[i]
		r.Key, r.Kind, r.Severity = it.Key, it.Kind, it.Severity
		r.Rig, r.Bead, r.SHA, r.Summary = it.Rig, it.Bead, it.SHA, it.Summary
		if !it.FirstSeen.IsZero() {
			r.FirstSeen = it.FirstSeen
		}
		if !it.LastSeen.IsZero() {
			r.LastSeen = it.LastSeen
		}
		if it.AckedAt != nil {
			r.AckedAt = it.AckedAt
			r.Acked = true
		}
	}

	for i := range out {
		r := &out[i]
		if r.Acked { // state.json already records the ack that hid this item.
			continue
		}
		// An ack made after a clear is not in force: the item was cleared, not
		// acted on, so only an ack up to the clear can hide it.
		at, ok := ackAt(acks, r.Key, r.ClearedAt)
		if !ok || at.Before(r.FirstSeen) {
			// An ack older than the life belongs to an earlier one.
			continue
		}
		r.AckedAt, r.Acked = &at, true
	}

	// A malformed log can raise a key twice without a clear; only the last
	// life can be the standing one, so rank the earlier ones before it.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.FirstSeen.Equal(b.FirstSeen) {
			return a.FirstSeen.Before(b.FirstSeen)
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if (a.ClearedAt == nil) != (b.ClearedAt == nil) {
			return a.ClearedAt != nil
		}
		return false
	})
	return out
}

// ackAt returns the latest time key was acknowledged, ignoring an ack later
// than notAfter when one is given.
func ackAt(acks Acks, key string, notAfter *time.Time) (time.Time, bool) {
	var at time.Time
	found := false
	for _, a := range acks.Acks {
		if a.Key != key || (notAfter != nil && a.At.After(*notAfter)) {
			continue
		}
		if !found || a.At.After(at) {
			at, found = a.At, true
		}
	}
	return at, found
}
