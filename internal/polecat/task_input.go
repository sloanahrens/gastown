package polecat

import (
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
// order. Items and their box text come from beads.AcceptanceItems, the matcher
// behind the host's TickAcceptanceBox: a tick naming text read differently here
// would name no box the host can find. A list item with no checkbox is
// skipped.
func parseAcceptanceBoxes(text string) []taskfile.Box {
	var boxes []taskfile.Box
	for _, item := range beads.AcceptanceItems(text) {
		if !item.Boxed {
			continue
		}
		boxes = append(boxes, taskfile.Box{Text: item.Box, Ticked: item.Checked})
	}
	return boxes
}
