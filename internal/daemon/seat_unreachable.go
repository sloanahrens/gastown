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

// seatCheck is what one seat's polecat-health check gives the seat-unreachable
// tracker.
type seatCheck int

const (
	// seatChecked: the check asked the session, and the read it returned is
	// the answer.
	seatChecked seatCheck = iota
	// seatSkipped: the check left the seat before asking its session, so it
	// has no read; the tracker keeps whatever it already held for the seat.
	seatSkipped
	// seatUnhooked: the seat holds no hooked bead. Nothing rests on its
	// session, so the tracker is told nothing about it and any outage kept for
	// the seat ends (gt-b0y45y).
	seatUnhooked
)

// seatUnreachable names a seat whose session has answered nothing since Since,
// the bead it holds and the last error its check returned. Released marks an
// outage the daemon has already released the seat for (gt-wgim24). Branch is the
// branch the release read for the seat's work when it last asked its worktree, so
// a release retried after the sandbox release dropped the record can still name
// the branch origin is asked about (gt-35ihg5).
type seatUnreachable struct {
	Rig      string
	Polecat  string
	Bead     string
	Branch   string
	Since    time.Time
	Err      error
	Released bool
}

// seatOutage is one seat's first failed read, kept from one tick to the next.
// released records that the daemon released the seat while this outage was
// running: the seat is still unreadable (it answered nothing) but its bead has
// already gone back to the pool, so the release must not run again until the
// outage ends (gt-wgim24).
type seatOutage struct {
	bead     string
	since    time.Time
	err      error
	released bool
	// branch is the branch the release read for the seat's work, kept with the
	// outage so every tick of a retried release names the same one after the
	// sandbox release has dropped the record that named it (gt-35ihg5).
	branch string
}

// seatKey names the seat a first-failure time belongs to.
type seatKey struct{ rig, polecat string }

// trackSeatUnreachable decides which hooked seats have been unreadable for
// seatUnreachableAfter, from the first-failure times of the previous tick and
// this tick's reads at now.
//
// A failed read keeps the seat's first-failure time, starting it at now for a
// seat absent from prev, and takes this read's bead and error as the ones to
// name; it keeps too the record that this outage was already released, so a
// released seat is not released again on every tick, and the branch the release
// read, so a retried release still names it once the seat's worktree and record
// no longer answer (gt-35ihg5). One good read drops the seat, and with it that
// record, so a later failure waits out the five minutes again and is released
// afresh. The caller passes every hooked seat on every tick: a seat missing from
// reads is dropped, which is how a released seat stops alarming.
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
		released := false
		branch := ""
		if old, ok := prev[key]; ok && !old.since.After(now) {
			// The same outage: it keeps its start and, with it, the record that
			// it has already been released and the branch its release read. A new
			// outage (the seat read good in between, then failed again) starts
			// here and is released afresh.
			since = old.since
			released = old.released
			branch = old.branch
		}
		next[key] = seatOutage{bead: read.Bead, since: since, err: read.Err, released: released, branch: branch}
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
			Branch:  outage.branch,
			Since:   outage.since,
			Err:     outage.err,
		})
	}
	return next, alerts
}
