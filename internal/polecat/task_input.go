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

// noteLines splits a bead's notes text into one note per non-blank line: the
// notes field is a block of text, and a line is the one entry this read can
// tell apart. A blank line carries no text and is dropped, so an empty notes
// field yields no notes at all.
func noteLines(notes string) []string {
	var lines []string
	for _, line := range strings.Split(notes, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// taskComments maps the bead's comments onto TASK.md's, in the order the client
// returns them. A created_at that does not parse leaves the time out of that
// comment's line rather than emptying the file: the author and the text, which
// is what the seat reads, are still carried.
func taskComments(comments []beads.Comment) []beadTaskComment {
	out := make([]beadTaskComment, 0, len(comments))
	for _, c := range comments {
		out = append(out, beadTaskComment{Author: c.Author, Time: commentTimeOrZero(c.CreatedAt), Text: c.Text})
	}
	return out
}

// commentTimeOrZero is a comment's created_at, which bd writes in RFC 3339. A
// value the layout rejects is the zero time, and taskfile writes no time for a
// zero one, so the comment keeps its author and text.
func commentTimeOrZero(created string) time.Time {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil {
		return time.Time{}
	}
	return at
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
