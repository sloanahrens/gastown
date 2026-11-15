package daemon

import (
	"maps"
	"time"
)

// seatUnreachableAfter is how long a hooked seat's session may stay unreadable
// before it is named (gt-l37xdn).
const seatUnreachableAfter = 5 * time.Minute

// seatRead is one hooked seat's session-check result for one patrol tick: the
// bead the seat holds, and the error the check returned, nil when the seat
// answered.
type seatRead struct {
	Rig     string
	Polecat string
	Bead    string
	Err     error
}

// seatUnreachable names a seat whose session has answered nothing since Since,
// the bead it holds and the last error its check returned.
type seatUnreachable struct {
	Rig     string
	Polecat string
	Bead    string
	Since   time.Time
	Err     error
}

// seatOutage is one seat's first failed read, kept from one tick to the next.
type seatOutage struct {
	bead  string
	since time.Time
	err   error
}

// seatKey names the seat a first-failure time belongs to.
type seatKey struct{ rig, polecat string }

// trackSeatUnreachable decides which hooked seats have been unreadable for
// seatUnreachableAfter, from the first-failure times of the previous tick and
// this tick's reads at now.
//
// A failed read keeps the seat's first-failure time, starting it at now for a
// seat absent from prev, and takes this read's bead and error as the ones to
// name. One good read drops the seat, so a later failure waits out the five
// minutes again. The caller passes every hooked seat on every tick: a seat
// missing from reads is dropped, which is how a released seat stops alarming.
// listed is false when the caller could not read the list of hooked seats at
// all: the tick then says nothing about any seat, prev is carried over as it
// is and nothing is named, so a failed or empty list read cannot clear alarms
// that are running. A list that was read and is empty is a real answer: no
// seat is hooked, every seat was released, so every outage is dropped.
// Reads that name one seat twice take the last read of it.
// A first-failure time later than now is a clock that stepped back, and the
// read restarts it at now rather than suppressing the alert until the wall
// clock catches up. prev is left unchanged.
func trackSeatUnreachable(prev map[seatKey]seatOutage, now time.Time, reads []seatRead, listed bool) (map[seatKey]seatOutage, []seatUnreachable) {
	if !listed {
		return maps.Clone(prev), nil
	}
	next := make(map[seatKey]seatOutage, len(reads))
	order := make([]seatKey, 0, len(reads))
	seen := make(map[seatKey]bool, len(reads))
	for _, read := range reads {
		key := seatKey{rig: read.Rig, polecat: read.Polecat}
		if read.Err == nil {
			delete(next, key)
			continue
		}
		since := now
		if old, ok := prev[key]; ok && !old.since.After(now) {
			since = old.since
		}
		next[key] = seatOutage{bead: read.Bead, since: since, err: read.Err}
		if !seen[key] {
			seen[key] = true
			order = append(order, key)
		}
	}
	var alerts []seatUnreachable
	for _, key := range order {
		outage, ok := next[key]
		if !ok || now.Sub(outage.since) < seatUnreachableAfter {
			continue
		}
		alerts = append(alerts, seatUnreachable{
			Rig:     key.rig,
			Polecat: key.polecat,
			Bead:    outage.bead,
			Since:   outage.since,
			Err:     outage.err,
		})
	}
	return next, alerts
}
