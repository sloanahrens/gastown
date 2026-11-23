package daemon

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/seatship"
)

// readSeatEndRecord returns the newest SEAT-END comment on bead that names rig,
// polecat and bead, found false when none; other comments skipped, read errors returned.
func readSeatEndRecord(client beads.Client, rig, polecat, bead string) (seatship.Payload, bool, error) {
	comments, err := client.Comments(bead)
	if err != nil {
		return seatship.Payload{}, false, err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		p, found, err := seatship.Decode(comments[i].Text)
		if err != nil || !found || p.Rig != rig || p.Polecat != polecat || p.Bead != bead {
			continue
		}
		return p, true, nil
	}
	return seatship.Payload{}, false, nil
}
