package polecat

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// beadTaskFields is the slice of a bead that TASK.md carries, already read by
// the caller: taskInputFromBead does no I/O, so the same fields always build
// the same Input.
type beadTaskFields struct {
	ID          string
	Title       string
	Description string
	Acceptance  string
	Notes       []string
	Comments    []beadTaskComment
}

// beadTaskComment is one bead comment as TASK.md shows it.
type beadTaskComment struct {
	Author string
	Time   time.Time
	Text   string
}

// taskInputFromBead maps a bead's fields onto the Input that taskfile.Assemble
// renders. The bead id is carried because a seat with no bead access names its
// bead in the outbox from TASK.md alone. Steps stay empty: the formula's steps
// are not the bead's fields.
func taskInputFromBead(f beadTaskFields) taskfile.Input {
	in := taskfile.Input{
		BeadID:      f.ID,
		Title:       f.Title,
		Description: f.Description,
		Boxes:       parseAcceptanceBoxes(f.Acceptance),
	}
	for _, note := range f.Notes {
		in.Notes = append(in.Notes, taskfile.Note{Text: note})
	}
	for _, c := range f.Comments {
		in.Comments = append(in.Comments, taskfile.Comment{Author: c.Author, Time: c.Time, Text: c.Text})
	}
	return in
}

// parseAcceptanceBoxes returns the checkbox items of an acceptance field, in
// order. Items come from beads.AcceptanceItems, the same matcher the host's
// ticker uses, and each box's text is trimmed as that ticker trims it: were
// the two to read different text, a seat's tick would name a box the host
// cannot find. A line that is not a list item opening with a "[ ]" or "[x]"
// box is skipped.
func parseAcceptanceBoxes(text string) []taskfile.Box {
	var boxes []taskfile.Box
	for _, item := range beads.AcceptanceItems(text) {
		if !item.Boxed {
			continue
		}
		// A list item's box is the line's first "[": the item's marker holds
		// none, so the box opens the text AcceptanceItems checked.
		at := strings.IndexByte(item.Line, '[')
		boxes = append(boxes, taskfile.Box{
			Text:   strings.TrimSpace(item.Line[at+3:]),
			Ticked: item.Checked,
		})
	}
	return boxes
}
