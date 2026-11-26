package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// movedOnLogPrefix namespaces this pass's keep-log keys: the gateway-silent
// drop pass logs a seat's keep under its bare rig/name, and one seat's line
// must not suppress the other pass's.
const movedOnLogPrefix = "moved-on\x00"

// movedOnKeep reports whether rec is a keep the branch guard wrote for work
// that landed: the only kind this pass drops (gt-i72us8).
func movedOnKeep(rec keptSandboxSeatRecord) bool {
	return rec.Landing != nil && rec.Landing.Landed &&
		strings.HasPrefix(rec.Reason, seatWorksKeepPrefix)
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

// dropMovedOnKeptSeats drops the kept record of every seat that has moved on
// to another bead (gt-qrzhot, gt-i72us8). The branch guard keeps a landed seat
// while it works its own polecat branch rather than the branch that landed; a
// seat slung to another bead afterwards never lands the kept bead, so its
// record can never be satisfied and would keep raising an attention item for a
// seat that is gone. A record this pass does not drop keeps it: one for another
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
	read := 0
	for _, seat := range seats {
		if !strings.HasPrefix(seat.Reason, seatWorksKeepPrefix) {
			continue
		}
		if read == budget {
			h.d.logger.Printf("patrol_scan: moved-on kept-record pass stopped at its budget of %d reads; the rest wait for the next tick", budget)
			return
		}
		read++
		h.dropMovedOnKeptSeat(seat.Rig, seat.Name)
	}
}

// dropMovedOnKeptSeat judges one seat's kept record and, when the seat works
// another bead than the one that landed, drops it under the seat lock. A keep
// it cannot settle logs once, so a seat whose agent bead stays unreadable is
// not re-reported on every tick.
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
		delete(h.d.keptDropLogged, key)
		h.d.logger.Printf("patrol_scan: kept record of %s/%s dropped: the seat works another bead than the landed %s", rig, name, landed)
		return nil
	})
	if err != nil {
		h.keptDropLogOnce(key, fmt.Sprintf("patrol_scan: kept record of %s/%s kept: %v", rig, name, err))
	}
}
