package outbox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// Store is the part of the bead store that Apply writes an outbox through. The
// host supplies the real store; the tests supply a fake.
type Store interface {
	// AppendNotes appends text to beadID's notes.
	AppendNotes(beadID, text string) error
	// SetAcceptance replaces beadID's acceptance text.
	SetAcceptance(beadID, text string) error
	// AddComment adds a comment from author to beadID.
	AddComment(beadID, author, text string) error
	// SetStatus sets beadID's status to status.
	SetStatus(beadID, status string) error
}

// Apply writes the parts of r that change the bead through store: the seat's
// notes, the acceptance boxes it ticked, its findings and a deferred status.
// actor is the seat's name, the author every finding is commented under.
//
// A deferred result sets the bead's status to deferred, after the other writes.
// A done result sets no status: the landing path owns the status of a task it
// lands.
//
// The writes run in that order. Every one is attempted even after an earlier
// one failed, so one failed write neither hides the writes after it nor reads
// as success: Apply returns all the failures joined, and nil only when every
// write it made succeeded. A status other than StatusDone or StatusDeferred,
// which Validate does not return, is refused into that joined error with no
// status written: a hand-built Result must not skip the status and read as a
// clean run.
//
// r.Ticked holds zero-based indexes into the checkbox lines of acceptance, read
// in order by beads.AcceptanceBoxes: an index outside that list is an error and
// no acceptance text is written for it. The acceptance is written only when
// ticking changed it, and a note only when r.Notes is not empty.
//
// The escalation request is not applied here.
func Apply(store Store, beadID, actor, acceptance string, r Result) error {
	var errs []error
	if r.Notes != "" {
		if err := store.AppendNotes(beadID, r.Notes); err != nil {
			errs = append(errs, fmt.Errorf("outbox: append notes to %s: %w", beadID, err))
		}
	}
	if len(r.Ticked) > 0 {
		updated, err := ticked(acceptance, r.Ticked)
		if err != nil {
			errs = append(errs, fmt.Errorf("outbox: tick acceptance of %s: %w", beadID, err))
		} else if updated != acceptance {
			if err := store.SetAcceptance(beadID, updated); err != nil {
				errs = append(errs, fmt.Errorf("outbox: set acceptance of %s: %w", beadID, err))
			}
		}
	}
	for i, finding := range r.Findings {
		if err := store.AddComment(beadID, actor, finding); err != nil {
			errs = append(errs, fmt.Errorf("outbox: add finding %d to %s: %w", i, beadID, err))
		}
	}
	switch r.Status {
	case StatusDone:
		// The landing path sets the status of a task it lands.
	case StatusDeferred:
		if err := store.SetStatus(beadID, string(StatusDeferred)); err != nil {
			errs = append(errs, fmt.Errorf("outbox: set status of %s: %w", beadID, err))
		}
	default:
		errs = append(errs, fmt.Errorf("outbox: set status of %s: result status %q is neither %q nor %q", beadID, r.Status, StatusDone, StatusDeferred))
	}
	return errors.Join(errs...)
}

// ticked returns acceptance with the checkbox lines named by ticks ticked.
// ticks are zero-based indexes into the checkbox lines of acceptance, read in
// order by beads.AcceptanceBoxes, which carries each box's own line, so a box
// is always ticked where it sits. Only the " " of a box changes: every other
// byte is kept, and a box that is already ticked is left alone. An index outside
// the list is an error and no text is returned for the acceptance.
func ticked(acceptance string, ticks []int) (string, error) {
	boxes := beads.AcceptanceBoxes(acceptance)
	lines := strings.Split(acceptance, "\n")
	changed := false
	for _, i := range ticks {
		if i < 0 || i >= len(boxes) {
			return "", fmt.Errorf("tick index %d is not one of the %d checkbox lines", i, len(boxes))
		}
		box := boxes[i]
		if box.Checked {
			continue
		}
		line := lines[box.Line]
		lines[box.Line] = line[:box.At+1] + "x" + line[box.At+2:]
		boxes[i].Checked = true
		changed = true
	}
	if !changed {
		return acceptance, nil
	}
	return strings.Join(lines, "\n"), nil
}
