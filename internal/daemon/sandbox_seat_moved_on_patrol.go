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
