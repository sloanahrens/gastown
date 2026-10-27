package events

import "time"

// DispatchStart returns when bead's current run was dispatched: the earliest
// sling event for the bead at or after its last run-end event. The clock
// belongs to the bead, not to one seat, so a bead handed from one seat to
// another keeps the dispatch it first went out on and cannot restart its clock
// by moving. A run-end event closes the run, so a bead dispatched again after
// one starts over: TypeDone, when the seat submitted the work (and so also a
// landed bead, whose landing follows its gt done), TypeBeadClosed, when the
// bead was closed by hand, and TypeOverrunStop, when the overrun guard stopped
// the seat and handed the bead back (a stop ends that run; the dispatch after
// it is policed afresh).
//
// The second return is false when the log holds no such sling.
func DispatchStart(evs []Event, bead string) (time.Time, bool) {
	c := NewDispatchClock()
	c.Add(evs)
	return c.Start(bead)
}

// DispatchStarts returns DispatchStart for every bead the log dispatched, so a
// reader that asks about many beads scans the log once instead of per bead.
func DispatchStarts(evs []Event) map[string]time.Time {
	c := NewDispatchClock()
	c.Add(evs)
	return c.Starts()
}

// DispatchClock is the dispatch clock built from events added in log order. It
// holds what DispatchStart reads, one event at a time, so a reader that polls
// the log can keep the clock current from the lines appended since its last
// read instead of re-reading the whole file (the dashboard's summary poll,
// gt-tzbtf).
type DispatchClock struct {
	// ends is each bead's latest run-end event, starts the dispatch its current
	// run began with (the earliest sling at or after ends, when there is one).
	ends   map[string]time.Time
	starts map[string]time.Time
}

// NewDispatchClock returns an empty clock.
func NewDispatchClock() *DispatchClock {
	return &DispatchClock{ends: map[string]time.Time{}, starts: map[string]time.Time{}}
}

// Add folds evs into the clock, in log order. Adding the same events again
// changes nothing: an end is only a boundary, and a dispatch only wins when it
// is earlier than the one held.
func (c *DispatchClock) Add(evs []Event) {
	for _, ev := range evs {
		bead := payloadBead(ev)
		if bead == "" {
			continue
		}
		switch ev.Type {
		case TypeSling:
			ts, err := eventTime(ev)
			if err != nil {
				continue
			}
			// A dispatch before the run's end belongs to the run that ended.
			if end, ok := c.ends[bead]; ok && ts.Before(end) {
				continue
			}
			if cur, ok := c.starts[bead]; !ok || ts.Before(cur) {
				c.starts[bead] = ts
			}
		case TypeDone, TypeBeadClosed, TypeOverrunStop:
			at, err := runEndTime(ev)
			if err != nil || !at.After(c.ends[bead]) {
				continue
			}
			c.ends[bead] = at
			// The boundary is inclusive on the dispatch side: a sling dated
			// exactly at the run end is the new run's dispatch, not the old
			// one's, because sling and end are written in whole seconds and an
			// end written in the same second must not leave the bead without a
			// clock. So only a dispatch strictly before the end is dropped.
			if start, ok := c.starts[bead]; ok && start.Before(at) {
				delete(c.starts, bead)
			}
		}
	}
}

// Start returns when bead's current run was dispatched, false when no sling
// since its last run end names it.
func (c *DispatchClock) Start(bead string) (time.Time, bool) {
	t, ok := c.starts[bead]
	return t, ok
}

// Starts returns a copy of every bead's dispatch, so a caller cannot change the
// clock through the map it is handed.
func (c *DispatchClock) Starts() map[string]time.Time {
	out := make(map[string]time.Time, len(c.starts))
	for bead, at := range c.starts {
		out[bead] = at
	}
	return out
}

// eventTime is when ev was written, its own timestamp. An event whose
// timestamp does not parse dates nothing: the log is a best-effort record, and
// a clock read from a malformed time would stop or restart the wrong seat.
func eventTime(ev Event) (time.Time, error) {
	return time.Parse(time.RFC3339, ev.Timestamp)
}

// runEndTime is when ev ended the bead's run: its "at" payload when the writer
// recorded a time of its own, else its timestamp. The overrun guard's stop
// writes "at", so a record repeated on a later tick (a hand-back that keeps
// failing) carries the same end each time and cannot creep past the dispatch it
// ends.
func runEndTime(ev Event) (time.Time, error) {
	if at, ok := ev.Payload["at"].(string); ok && at != "" {
		if ts, err := time.Parse(time.RFC3339, at); err == nil {
			return ts, nil
		}
	}
	return eventTime(ev)
}

// payloadBead is the bead an event names, "" when it names none.
func payloadBead(ev Event) string {
	bead, _ := ev.Payload["bead"].(string)
	return bead
}
