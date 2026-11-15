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
}

// Apply writes the parts of r that change the bead through store: the seat's
// notes, the acceptance boxes it ticked and its findings. actor is the seat's
// name, the author every finding is commented under.
//
// The writes run in that order. Every one is attempted even after an earlier
// one failed, so one failed write neither hides the writes after it nor reads
// as success: Apply returns all the failures joined, and nil only when every
// write it made succeeded.
//
// r.Ticked holds zero-based indexes into the checkbox lines of acceptance, read
// in order by beads.AcceptanceItems: an index outside that list is an error and
// no acceptance text is written for it. The acceptance is written only when
// ticking changed it, and a note only when r.Notes is not empty.
//
// The status and the escalation request are not applied here.
func Apply(store Store, beadID, actor, acceptance string, r Result) error {
	var errs []error
	if r.Notes != "" {
		if err := store.AppendNotes(beadID, r.Notes); err != nil {
			errs = append(errs, fmt.Errorf("outbox: append notes to %s: %w", beadID, err))
		}
	}
	if len(r.Ticked) > 0 {
		ticked, err := ticked(acceptance, r.Ticked)
		if err != nil {
			errs = append(errs, fmt.Errorf("outbox: tick acceptance of %s: %w", beadID, err))
		} else if ticked != acceptance {
			if err := store.SetAcceptance(beadID, ticked); err != nil {
				errs = append(errs, fmt.Errorf("outbox: set acceptance of %s: %w", beadID, err))
			}
		}
	}
	for i, finding := range r.Findings {
		if err := store.AddComment(beadID, actor, finding); err != nil {
			errs = append(errs, fmt.Errorf("outbox: add finding %d to %s: %w", i, beadID, err))
		}
	}
	return errors.Join(errs...)
}

// checkbox is one checkbox line of an acceptance text: the index of the line it
// sits on, the byte offset of its "[" in that line, and whether it is already
// ticked.
type checkbox struct {
	line    int
	at      int
	checked bool
}

// ticked returns acceptance with the checkbox lines named by ticks ticked.
// ticks are zero-based indexes into the checkbox lines of acceptance, read in
// order by beads.AcceptanceItems. Only the " " of a box changes: every other
// byte is kept, and a box that is already ticked is left alone. An index outside
// the list is an error and no text is returned for the acceptance.
func ticked(acceptance string, ticks []int) (string, error) {
	boxes := checkboxes(acceptance)
	lines := strings.Split(acceptance, "\n")
	changed := false
	for _, i := range ticks {
		if i < 0 || i >= len(boxes) {
			return "", fmt.Errorf("tick index %d is not one of the %d checkbox lines", i, len(boxes))
		}
		box := boxes[i]
		if box.checked {
			continue
		}
		line := lines[box.line]
		lines[box.line] = line[:box.at+1] + "x" + line[box.at+2:]
		boxes[i].checked = true
		changed = true
	}
	if !changed {
		return acceptance, nil
	}
	return strings.Join(lines, "\n"), nil
}

// checkboxes returns the position of every checkbox line of acceptance, in the
// order beads.AcceptanceItems reads them. A code fence is example text, and a
// fence opens only when a later line closes it, the rule AcceptanceItems uses,
// so an unfinished fence cannot hide a checkbox line from it.
//
// Every boxed item is paired with its line: the walk reads the lines outside
// fenced blocks with the same rule AcceptanceItems does, and outside a fence a
// line with a boxed item's text is that item, so no box is left unplaced.
func checkboxes(acceptance string) []checkbox {
	var wanted []beads.AcceptanceItem
	for _, item := range beads.AcceptanceItems(acceptance) {
		if item.Boxed {
			wanted = append(wanted, item)
		}
	}
	lines := strings.Split(acceptance, "\n")
	boxes := make([]checkbox, 0, len(wanted))
	fence := ""
	next := 0
	for i := 0; i < len(lines) && next < len(wanted); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if fence != "" {
			if marker := beads.FenceMarker(trimmed); marker != "" && strings.HasPrefix(marker, fence) {
				fence = ""
			}
			continue
		}
		if marker := beads.FenceMarker(trimmed); marker != "" && fenceClosesAfter(lines[i+1:], marker) {
			fence = marker
			continue
		}
		if trimmed != wanted[next].Line {
			continue
		}
		boxes = append(boxes, checkbox{line: i, at: strings.Index(lines[i], "["), checked: wanted[next].Checked})
		next++
	}
	return boxes
}

// fenceClosesAfter reports whether a line of rest closes the fence marker opens:
// the same character, at least as long.
func fenceClosesAfter(rest []string, marker string) bool {
	for _, line := range rest {
		if closer := beads.FenceMarker(strings.TrimSpace(line)); closer != "" && strings.HasPrefix(closer, marker) {
			return true
		}
	}
	return false
}
