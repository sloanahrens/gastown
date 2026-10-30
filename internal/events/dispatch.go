package events

import (
	"sort"
	"time"
)

// specDispatchActor is the actor the spec dispatcher stamps its slings with,
// repeating internal/cmd's specDispatchActor; it is named here because a sling
// it wrote starts a fresh dispatch run (DispatchStart, gt-3ga5l).
const specDispatchActor = "daemon/spec-dispatch"

// DispatchStart returns when bead's current run was dispatched: the earliest
// sling event for the bead at or after its last run-end event. The clock
// belongs to the bead, not to one seat, so a bead handed from one seat to
// another keeps the dispatch it first went out on and cannot restart its clock
// by moving. A run-end event closes the run, so a bead dispatched again after
// one starts over: TypeDone, when the seat submitted the work (and so also a
// landed bead, whose landing follows its gt done), TypeBeadClosed, when the
// bead was closed by hand, and TypeOverrunStop, when the overrun guard stopped
// the seat and handed the bead back (a stop ends that run; the dispatch after
// it is policed afresh). A sling by the spec dispatcher ends the run before it
// too, though no event recorded that end: it only slings a bead that is back in
// the pool, so the run its earlier sling began has no seat and its time is not
// the new seat's to answer for (gt-3ga5l).
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
	// ends is each bead's latest run end. starts holds the slings since it, in
	// time order: the earliest is the clock, and a later one is what a run end
	// recorded outside the log (StartSince) exposes once it drops the earlier
	// dispatches. A list holds the dispatches the log still carries for a bead
	// whose runs are not ended, which is a handful for any bead the log keeps.
	ends   map[string]time.Time
	starts map[string][]time.Time
}

// NewDispatchClock returns an empty clock.
func NewDispatchClock() *DispatchClock {
	return &DispatchClock{ends: map[string]time.Time{}, starts: map[string][]time.Time{}}
}

// Add folds evs into the clock, in log order. Adding the same events again
// changes nothing: a sling already filed is not filed twice, and an end is
// only a boundary. A spec-dispatch sling ends the bead's run before it first,
// so it dates the run it starts (DispatchStart).
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
			if ev.Actor == specDispatchActor {
				c.EndRun(bead, ts)
			}
			c.addStart(bead, ts)
		case TypeDone, TypeBeadClosed, TypeOverrunStop:
			at, err := runEndTime(ev)
			if err != nil {
				continue
			}
			c.EndRun(bead, at)
		}
	}
}

// addStart files ts among bead's dispatches since its last run end. A sling
// before the end belongs to the run that ended and is not filed at all.
func (c *DispatchClock) addStart(bead string, ts time.Time) {
	if end, ok := c.ends[bead]; ok && ts.Before(end) {
		return
	}
	s := c.starts[bead]
	i := sort.Search(len(s), func(i int) bool { return !s[i].Before(ts) })
	if i < len(s) && s[i].Equal(ts) {
		return // the same event folded in twice
	}
	s = append(s, time.Time{})
	copy(s[i+1:], s[i:])
	s[i] = ts
	c.starts[bead] = s
}

// EndRun records that bead's run ended at at, even when the log holds no event
// for it: a submission for landing is written on the bead's notes, and its done
// event can be lost (gt-chd44). A dispatch strictly before the end belongs to
// the run it ended; one dated exactly at it is the new run's, so a bead is never
// left without a clock, and sling and end are written in whole seconds. An end
// not after the one held changes nothing, so replaying a log or re-reading a
// bead cannot move a boundary backwards.
func (c *DispatchClock) EndRun(bead string, at time.Time) {
	if at.IsZero() || !at.After(c.ends[bead]) {
		return
	}
	c.ends[bead] = at
	s := c.starts[bead]
	if i := sort.Search(len(s), func(i int) bool { return !s[i].Before(at) }); i > 0 {
		s = s[i:]
	}
	if len(s) == 0 {
		delete(c.starts, bead)
		return
	}
	c.starts[bead] = s
}

// StartSince returns when bead's current run was dispatched, with end taken as
// a run end the log does not hold (EndRun), so a rework that follows a landing
// rejection is dated from its re-sling rather than the dispatch the submitted
// run began with (gt-chd44). A bead whose only sling is before end is dated from
// end until the re-sling it waits for is logged; one the log never dispatched
// has no clock from this call, whatever the end.
func (c *DispatchClock) StartSince(bead string, end time.Time) (time.Time, bool) {
	start, ok := c.Start(bead)
	if end.IsZero() || (ok && !start.Before(end)) {
		return start, ok
	}
	if !ok {
		return time.Time{}, false
	}
	c.EndRun(bead, end)
	if rework, found := c.Start(bead); found {
		return rework, true
	}
	return end, true
}

// Start returns when bead's current run was dispatched, false when no sling
// since its last run end names it.
func (c *DispatchClock) Start(bead string) (time.Time, bool) {
	s := c.starts[bead]
	if len(s) == 0 {
		return time.Time{}, false
	}
	return s[0], true
}

// Starts returns a copy of every bead's dispatch, so a caller cannot change the
// clock through the map it is handed.
func (c *DispatchClock) Starts() map[string]time.Time {
	out := make(map[string]time.Time, len(c.starts))
	for bead, s := range c.starts {
		if len(s) > 0 {
			out[bead] = s[0]
		}
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
