package events

import "time"

// DispatchStart returns when bead's current run was dispatched: the earliest
// sling event for the bead at or after its last done event. The clock belongs
// to the bead, not to one seat, so a bead handed from one seat to another
// keeps the dispatch it first went out on and cannot restart its clock by
// moving. A done event ends the run — the work was submitted, landed or
// closed — so a bead dispatched again after it starts over. The second return
// is false when the log holds no such sling.
//
// Events are dated by their own timestamps; one that does not parse is
// skipped, since the log is a best-effort record.
func DispatchStart(evs []Event, bead string) (time.Time, bool) {
	if bead == "" {
		return time.Time{}, false
	}
	var ended time.Time
	for _, ev := range evs {
		if ev.Type != TypeDone || payloadBead(ev) != bead {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, ev.Timestamp); err == nil && ts.After(ended) {
			ended = ts
		}
	}
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
	ended := map[string]time.Time{}
	for _, ev := range evs {
		if ev.Type != TypeDone {
			continue
		}
		bead := payloadBead(ev)
		if bead == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, ev.Timestamp); err == nil && ts.After(ended[bead]) {
			ended[bead] = ts
		}
	}
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

// payloadBead is the bead an event names, "" when it names none.
func payloadBead(ev Event) string {
	bead, _ := ev.Payload["bead"].(string)
	return bead
}
