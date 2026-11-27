package daemon

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// movedOnLogPrefix namespaces this pass's keep-log keys: the gateway-silent
// drop pass logs a seat's keep under its bare rig/name, and one seat's line
// must not suppress the other pass's.
const movedOnLogPrefix = "moved-on\x00"

// movedOnKeepReason reports whether a keep reason is one this pass judges: the
// branch guard's keep of a seat working its own branch, or the gateway-silent
// keep, since a seat slung to another bead leaves either one unsatisfiable.
func movedOnKeepReason(reason string) bool {
	return strings.HasPrefix(reason, seatWorksKeepPrefix) || isGatewayNoAnswerKeep(reason)
}

// movedOnKeep reports whether rec is a keep this pass may drop: work that
// landed, for one of the reasons movedOnKeepReason judges (gt-i72us8,
// gt-29kb5p).
func movedOnKeep(rec keptSandboxSeatRecord) bool {
	return rec.Landing != nil && rec.Landing.Landed && movedOnKeepReason(rec.Reason)
}

// movedOnSeat reads the kept record at path and the seat's agent bead and
// reports whether the seat works another bead than the one that landed, and
// which bead landed. A record an earlier pass removed, one for another keep,
// and a record or bead that cannot be read are no moved-on keep; an error is a
// read that failed, never a keep.
func (h *patrolScanHost) movedOnSeat(rig, name, path string) (movedOn bool, landed string, err error) {
	rec, err := readKeptSandboxSeatRecord(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil // an earlier pass or the seat's end dropped it
		}
		return false, "", fmt.Errorf("cannot re-read its kept record: %w", err)
	}
	if !movedOnKeep(rec) {
		return false, "", nil
	}
	fields, err := h.seatAgentFields(rig, name)
	if err != nil {
		return false, "", fmt.Errorf("cannot read its agent bead: %w", err)
	}
	return fields != nil && seatMovedOn(strings.TrimSpace(fields.Branch), rec.Landing.BeadID), rec.Landing.BeadID, nil
}

// movedOnAfter reports whether seat sorts after the rig/name cursor in the order
// KeptSandboxSeats returns seats: by rig, then by name. Comparing the joined
// key would disagree with that order when one rig name prefixes another
// ("gastown" and "gastown-2": '-' sorts before '/'), and the search over the
// seats would then start behind the cursor's record (gt-qo9sb5).
func movedOnAfter(seat KeptSandboxSeat, cursor string) bool {
	rig, name, _ := strings.Cut(cursor, "/")
	if seat.Rig != rig {
		return seat.Rig > rig
	}
	return seat.Name > name
}

// dropMovedOnKeptSeats drops the kept record of every seat that has moved on
// to another bead (gt-qrzhot, gt-i72us8). The branch guard keeps a landed seat
// while it works its own polecat branch rather than the branch that landed, and
// a gateway-silent keep holds a landed seat whose gateway did not answer; a seat
// slung to another bead afterwards never lands the kept bead, so its record can
// never be satisfied and would keep raising an attention item for a seat that is
// gone (gt-29kb5p). A record this pass does not drop keeps it: one for another
// reason, one whose landing is unlanded, a seat still on the landed bead (a
// takeover), a record or agent bead that cannot be read, and a removal that
// fails.
//
// Every record is judged twice: here, and again inside the seat lock the sling
// and the seat's end take, where the record and the agent bead are re-read. A
// seat taken over to the landed bead in between therefore keeps its record.
//
// One tick reads at most the record walk's budget of kept records, so a town
// with many moved-on keeps does not ask the agent store for every seat in one
// tick. The records it does not reach wait for the next tick, and the stop is
// logged with the budget spent (gt-52noj3).
//
// The tick starts at the first record after the one the last tick read, kept as
// Daemon.movedOnCursor, so a record the pass reads and leaves cannot hold the
// first read of every tick: the records behind it are still reached within a
// bounded number of ticks (gt-qo9sb5).
func (h *patrolScanHost) dropMovedOnKeptSeats() {
	seats, _, err := KeptSandboxSeats(h.town())
	if err != nil {
		h.d.logger.Printf("patrol_scan: the kept-seat records cannot be read, so none is dropped: %v", err)
		return
	}
	budget := h.sandboxRecordBudget
	if budget <= 0 {
		budget = sandboxRecordsPerTick
	}
	n := len(seats)
	// The start is the first seat after the last record the previous tick read,
	// wrapping. Searching the key rather than an index keeps the walk rotating
	// when the cursor's record was dropped since.
	start := sort.Search(n, func(i int) bool { return movedOnAfter(seats[i], h.d.movedOnCursor) }) % max(n, 1)
	read := 0
	for i := 0; i < n; i++ {
		seat := seats[(start+i)%n]
		if !movedOnKeepReason(seat.Reason) {
			continue
		}
		if read == budget {
			h.d.logger.Printf("patrol_scan: moved-on kept-record pass stopped at its budget of %d reads; the rest wait for the next tick", budget)
			return
		}
		read++
		h.d.movedOnCursor = seat.Rig + "/" + seat.Name
		h.dropMovedOnKeptSeat(seat.Rig, seat.Name)
	}
}

// dropMovedOnKeptSeat judges one seat's kept record and, when the seat works
// another bead than the one that landed, drops it under the seat lock and
// clears the seat's silent-gateway tally, so a released seat raises no item on
// its own. A keep it cannot settle logs once, so a seat whose agent bead stays
// unreadable is not re-reported on every tick.
func (h *patrolScanHost) dropMovedOnKeptSeat(rig, name string) {
	key := movedOnLogPrefix + rig + "/" + name
	path, err := keptSandboxSeatPath(h.town(), rig, name)
	if err != nil {
		return // KeptSandboxSeats refused this name, so it has no record to read
	}
	movedOn, _, err := h.movedOnSeat(rig, name, path)
	if err != nil {
		h.keptDropLogOnce(key, fmt.Sprintf("patrol_scan: kept record of %s/%s kept: %v", rig, name, err))
		return
	}
	if !movedOn {
		return
	}
	err = h.withSeatLock(rig, name, func() error {
		movedOn, landed, err := h.movedOnSeat(rig, name, path)
		if err != nil {
			return err
		}
		if !movedOn {
			return nil // gone, another keep, or a takeover: nothing to drop
		}
		if err := h.removeKeptSeatRecord(rig, name); err != nil {
			return fmt.Errorf("its record cannot be removed: %w", err)
		}
		// The seat is released, so its silent-gateway count ends with the
		// record; a tally left behind would raise the seat on its own.
		h.d.clearGatewayTally(rig, name)
		delete(h.d.keptDropLogged, key)
		h.d.logger.Printf("patrol_scan: kept record of %s/%s dropped: the seat works another bead than the landed %s", rig, name, landed)
		return nil
	})
	if err != nil {
		h.keptDropLogOnce(key, fmt.Sprintf("patrol_scan: kept record of %s/%s kept: %v", rig, name, err))
	}
}
