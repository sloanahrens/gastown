package events

import (
	"sort"
	"time"
)

// SpecDispatchActor is the actor the spec dispatcher stamps its slings with,
// repeating internal/cmd's SpecDispatchActor (a test in internal/cmd holds the
// two equal); it is named here because a sling
// it wrote starts a fresh dispatch run (DispatchStart, gt-3ga5l).
const SpecDispatchActor = "daemon/spec-dispatch"

// DispatchStart returns when bead's current run was dispatched: the earliest
// sling event for the bead at or after its last run-end event. The clock
// belongs to the bead, not to one seat, so a bead handed from one seat to
// another keeps the dispatch it first went out on and cannot restart its clock
// by moving. A run-end event closes the run, so a bead dispatched again after
// one starts over: TypeDone, when the seat submitted the work (and so also a
// landed bead, whose landing follows its gt done), TypeBeadClosed, when the
// bead was closed by hand, and TypeOverrunStop, when the overrun guard stopped
// the seat and handed the bead back (a stop ends that run; the dispatch after
// it is policed afresh). A sling by the spec dispatcher to a DIFFERENT
// seat than the bead's last sling ends the run before it too, though no event
// recorded that end: the bead moved, so the run its earlier sling began has no
// seat and its time is not the new seat's to answer for (gt-3ga5l). A repeat
// sling to the seat that already holds the bead is the same run and keeps its
// first dispatch, so repeating a sling cannot restart the clock.
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
// gt-tzbtf). The run a bead's clock dates ends at a done, a close or an overrun
// stop, and when the spec dispatcher moves the bead to another seat.
type DispatchClock struct {
	// ends is each bead's latest run end. starts holds the slings since it, in
	// time order: the earliest is the clock, and a later one is what a run end
	// recorded outside the log (StartSince) exposes once it drops the earlier
	// dispatches. A list holds the dispatches the log still carries for a bead
	// whose runs are not ended, which is a handful for any bead the log keeps.
	ends   map[string]time.Time
	starts map[string][]dispatchPoint
	// targets is the seat each bead was last slung to, so a spec-dispatch sling
	// to a different seat can be told from a repeat to the seat that holds it.
	targets map[string]string
}

// dispatchPoint is one sling filed in a bead's start list: when it was written,
// and the seat it named ("" when it named none).
type dispatchPoint struct {
	at     time.Time
	target string
}

// NewDispatchClock returns an empty clock.
func NewDispatchClock() *DispatchClock {
	return &DispatchClock{ends: map[string]time.Time{}, starts: map[string][]dispatchPoint{}, targets: map[string]string{}}
}

// Add folds evs into the clock, in log order. Adding the same events again
// changes nothing: a sling already filed is not filed twice, and an end is
// only a boundary. A spec-dispatch sling to a different seat than the bead's
// last one ends the bead's run before it first, so it dates the run it starts
// (DispatchStart); a repeat to the same seat does not.
func (c *DispatchClock) Add(evs []Event) {
	for _, ev := range evs {
		c.add(ev)
	}
}

// add folds one event into the clock (Add).
func (c *DispatchClock) add(ev Event) {
	bead := payloadBead(ev)
	if bead == "" {
		return
	}
	switch ev.Type {
	case TypeSling:
		ts, err := eventTime(ev)
		if err != nil {
			return
		}
		target, _ := ev.Payload["target"].(string)
		if prev, ok := c.targets[bead]; ev.Actor == SpecDispatchActor && ok && target != "" && prev != target {
			c.EndRun(bead, ts)
		}
		if target != "" {
			c.targets[bead] = target
		}
		c.addStart(bead, ts, target)
	case TypeDone, TypeBeadClosed, TypeOverrunStop:
		at, err := runEndTime(ev)
		if err != nil {
			return
		}
		c.EndRun(bead, at)
	}
}

// DispatchedBy returns when the run bead was working under at end was
// dispatched, and the seat that sling named: the earliest sling event of the
// run the log holds strictly before end, with the run ends and seat moves
// before it folded in (DispatchStart). The run's own end event, written at or
// after end, is not folded in, so the clock names the run that was still
// current rather than the rework after it. The second return is "" when the
// sling named no seat, and the third is false when the log holds no such sling.
//
// A landing uses it to time a bead whose seat wrote no span start, where the
// dispatch is the earliest the seat can have been given the bead (gt-v7ptp).
func DispatchedBy(evs []Event, bead string, end time.Time) (time.Time, string, bool) {
	c := NewDispatchClock()
	for _, ev := range evs {
		if ts, err := eventTime(ev); err == nil && !ts.Before(end) {
			continue
		}
		c.add(ev)
	}
	return c.Dispatched(bead)
}

// addStart files ts, slung to target, among bead's dispatches since its last
// run end. A sling before the end belongs to the run that ended and is not
// filed at all.
func (c *DispatchClock) addStart(bead string, ts time.Time, target string) {
	if end, ok := c.ends[bead]; ok && ts.Before(end) {
		return
	}
	s := c.starts[bead]
	i := sort.Search(len(s), func(i int) bool { return !s[i].at.Before(ts) })
	if i < len(s) && s[i].at.Equal(ts) {
		return // the same event folded in twice
	}
	s = append(s, dispatchPoint{})
	copy(s[i+1:], s[i:])
	s[i] = dispatchPoint{at: ts, target: target}
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
	if i := sort.Search(len(s), func(i int) bool { return !s[i].at.Before(at) }); i > 0 {
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
	at, _, ok := c.Dispatched(bead)
	return at, ok
}

// Dispatched returns when bead's current run was dispatched and the seat that
// sling named, "" when it named none. The seat is the one the dispatch time
// belongs to, so a reader can tell whose clock it is reading.
func (c *DispatchClock) Dispatched(bead string) (time.Time, string, bool) {
	s := c.starts[bead]
	if len(s) == 0 {
		return time.Time{}, "", false
	}
	return s[0].at, s[0].target, true
}

// Starts returns a copy of every bead's dispatch, so a caller cannot change the
// clock through the map it is handed.
func (c *DispatchClock) Starts() map[string]time.Time {
	out := make(map[string]time.Time, len(c.starts))
	for bead, s := range c.starts {
		if len(s) > 0 {
			out[bead] = s[0].at
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
