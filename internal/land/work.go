package land

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

const (
	// LabelReadyToLand marks a work bead whose branch is pushed and waiting
	// for the landing worker. gt done sets it; Land removes it on every
	// outcome that writes the bead.
	LabelReadyToLand = "gt:ready-to-land"
	// LabelRework marks a work bead Land rejected for a reason its author can
	// fix. Dispatch already routes on it (sling_pool.go).
	LabelRework = "rework"
	// LabelNeedsHuman marks a work bead Land refused on policy only a human
	// can lift (no_merge).
	LabelNeedsHuman = "gt:needs-human"

	// ReadyNoteMarker opens the notes block gt done writes to say what to land.
	ReadyNoteMarker = "READY TO LAND"
)

// Work is one landing request: the work bead and the pushed branch head it
// declares.
type Work struct {
	BeadID string
	Rig    string
	Branch string
	// Head is the commit gt done pushed and verified on origin/<Branch>.
	// Land merges this commit, never whatever the branch holds later.
	Head   string
	Target string
	Worker string
	// Submitted is when the bead was submitted for landing, and is what the
	// landing worker orders the ready queue by: a comment on the bead is not
	// a submission and does not move it (gt-t2jhf). Zero on a bead submitted
	// before the note carried one.
	Submitted time.Time
}

// FormatReadyNote renders the READY TO LAND block for w. Every value is
// collapsed onto one line, so no field can inject a line the parser reads.
// Submitted is left out while it is zero.
func FormatReadyNote(w Work) string {
	note := fmt.Sprintf("%s\nBranch: %s\nHead: %s\nTarget: %s\nWorker: %s",
		ReadyNoteMarker, NoteField(w.Branch), NoteField(w.Head), NoteField(w.Target), NoteField(w.Worker))
	if !w.Submitted.IsZero() {
		note += "\nSubmitted: " + w.Submitted.UTC().Format(time.RFC3339)
	}
	return note
}

// ParseReadyNote reads the last READY TO LAND block in notes. A rework
// resubmission appends a new block, so the last one is the live request.
// BeadID and Rig are not part of the note and come back empty.
func ParseReadyNote(notes string) (Work, bool) {
	idx := strings.LastIndex(notes, ReadyNoteMarker+"\n")
	if idx < 0 {
		return Work{}, false
	}
	var w Work
	seen := map[string]bool{}
	for _, line := range strings.Split(notes[idx+len(ReadyNoteMarker)+1:], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if seen[key] {
			break
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		switch key {
		case "branch":
			w.Branch = value
		case "head":
			w.Head = value
		case "target":
			w.Target = value
		case "worker":
			w.Worker = value
		case "submitted":
			// A time this code cannot read orders as if the block carried
			// none: a bad stamp is not a reason to refuse to land the work.
			if at, err := time.Parse(time.RFC3339, value); err == nil {
				w.Submitted = at
			}
		default:
			return finishReady(w)
		}
	}
	return finishReady(w)
}

func finishReady(w Work) (Work, bool) {
	if w.Branch == "" || w.Head == "" || w.Target == "" {
		return Work{}, false
	}
	return w, true
}

// ErrNotReady means the bead does not carry a complete landing request.
var ErrNotReady = errors.New("work bead is not ready to land")

// WorkFromBead builds the landing request from a work bead: it must carry the
// ready label and a READY TO LAND block.
func WorkFromBead(issue *beads.Issue, rig string) (Work, error) {
	if issue == nil {
		return Work{}, fmt.Errorf("%w: no bead", ErrNotReady)
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return Work{}, fmt.Errorf("%w: %s has no %s label", ErrNotReady, issue.ID, LabelReadyToLand)
	}
	w, ok := ParseReadyNote(issue.Notes)
	if !ok {
		return Work{}, fmt.Errorf("%w: %s has no complete %s block in its notes", ErrNotReady, issue.ID, ReadyNoteMarker)
	}
	w.BeadID = issue.ID
	w.Rig = rig
	return w, nil
}

// NoteField collapses a free-text note field onto one line. Branch names,
// titles, paths and reasons are agent-supplied, and a newline in one of them
// would inject whole lines into the notes: a forged finding, a forged
// "MERGE REJECTION (attempt" marker, or a forged Head (gt-s4f6).
func NoteField(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
