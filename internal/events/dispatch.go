package events

import "time"

// DispatchStart returns when bead's current run was dispatched: the earliest
// sling event for the bead at or after its last run-end event. The clock
// belongs to the bead, not to one seat, so a bead handed from one seat to
// another keeps the dispatch it first went out on and cannot restart its clock
// by moving. A run-end event closes the run, so a bead dispatched again after
// one starts over: TypeDone, when the seat submitted, landed or closed the
// work, and TypeOverrunStop, when the overrun guard stopped the seat and handed
// the bead back (a stop ends that run; the dispatch after it is policed afresh).
// The second return is false when the log holds no such sling.
//
// The boundary is inclusive on the dispatch side: a sling dated exactly at the
// run-end event is the new run's dispatch, not the old run's, because sling and
// end are written in whole seconds and an end written in the same second must
// not leave the bead without a clock.
//
// Events are dated by their own timestamps; one that does not parse is
// skipped, since the log is a best-effort record.
func DispatchStart(evs []Event, bead string) (time.Time, bool) {
	if bead == "" {
		return time.Time{}, false
	}
	ended := runEnds(evs)[bead]
	var start time.Time
	for _, ev := range evs {
		if ev.Type != TypeSling || payloadBead(ev) != bead {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil || (!ended.IsZero() && ts.Before(ended)) {
			continue
		}
		if start.IsZero() || ts.Before(start) {
			start = ts
		}
	}
	return start, !start.IsZero()
}

// DispatchStarts returns DispatchStart for every bead the log dispatched, so a
// reader that asks about many beads scans the log once instead of per bead.
func DispatchStarts(evs []Event) map[string]time.Time {
	ended := runEnds(evs)
	out := map[string]time.Time{}
	for _, ev := range evs {
		if ev.Type != TypeSling {
			continue
		}
		bead := payloadBead(ev)
		if bead == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil {
			continue
		}
		if end, ok := ended[bead]; ok && ts.Before(end) {
			continue
		}
		if cur, ok := out[bead]; !ok || ts.Before(cur) {
			out[bead] = ts
		}
	}
	return out
}

// runEnds indexes each bead's latest run-end event: the seat finished the work
// (TypeDone) or the overrun guard ended it (TypeOverrunStop). The dispatch
// after that event is a new run, judged from its own start.
func runEnds(evs []Event) map[string]time.Time {
	out := map[string]time.Time{}
	for _, ev := range evs {
		if ev.Type != TypeDone && ev.Type != TypeOverrunStop {
			continue
		}
		bead := payloadBead(ev)
		if bead == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil || !ts.After(out[bead]) {
			continue
		}
		out[bead] = ts
	}
	return out
}

// payloadBead is the bead an event names, "" when it names none.
func payloadBead(ev Event) string {
	bead, _ := ev.Payload["bead"].(string)
	return bead
}
