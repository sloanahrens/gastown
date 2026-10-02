package land

import (
	"errors"
	"fmt"
	"regexp"
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
	// LabelNeedsHuman marks a work bead whose landing only a human can settle:
	// Land refused it on policy (no_merge), or the bead hit
	// MaxReworkAttempts and the rework loop is the failure (gt-28ibg).
	LabelNeedsHuman = "gt:needs-human"

	// LabelOverseerReviewed marks a work bead whose head the overseer
	// reviewed in place of om; it counts only beside an OverseerReviewedMarker
	// note naming that exact head (gt-g8t3m). gt done removes it on every
	// submission, so a new head is never waved through on an old review.
	LabelOverseerReviewed = "gt:overseer-reviewed"
	// LabelOverseerReviewWanted marks a work bead whose landing touched a
	// risk path (RiskPathsFile) and so wants a post-landing look at that head
	// (gt-vsct7.4). It is deliberately not LabelOverseerReviewed: that one
	// says the overseer stood in for om, this one asks for a human. Land adds
	// it and nothing strips it — a review does not remove it, and the
	// attention item it feeds clears on the review note (OverseerReviewMarker).
	LabelOverseerReviewWanted = "gt:overseer-review-wanted"

	// OverseerReviewMarker opens the note line "OVERSEER REVIEW <full head
	// sha> PASS|FAIL" that answers a risk-path item: it names the head a
	// human looked at, and PASS or FAIL says what they found. It is not
	// OverseerReviewedMarker — that marker says om was bypassed, and must not
	// satisfy a risk-path item (gt-vsct7.4).
	OverseerReviewMarker = "OVERSEER REVIEW"
	// OverseerReviewedMarker opens the note line "OVERSEER REVIEWED <full
	// head sha>" that binds the overseer's review to one head.
	OverseerReviewedMarker = "OVERSEER REVIEWED"

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

// OverseerReviewed reports whether the overseer reviewed head in place of om:
// the bead carries LabelOverseerReviewed and a notes line
// "OVERSEER REVIEWED <sha>" whose sha is the full head. A label alone, or a
// review of another head, is not one (gt-g8t3m).
func OverseerReviewed(issue *beads.Issue, head string) bool {
	if issue == nil || head == "" || !beads.HasLabel(issue, LabelOverseerReviewed) {
		return false
	}
	for _, line := range strings.Split(issue.Notes, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), OverseerReviewedMarker+" ")
		if ok && strings.TrimSpace(rest) == head {
			return true
		}
	}
	return false
}

// overseerReviewLine matches "OVERSEER REVIEW <sha> PASS|FAIL", the note that
// answers a risk-path item. The trailing \b keeps a word from extending a
// verdict ("PASSED" is not PASS), and the sha is compared whole, so a review
// of another head does not answer. It deliberately does not match
// "OVERSEER REVIEWED ...": that marker says om was bypassed, not that a human
// reviewed this head (gt-vsct7.4).
var overseerReviewLine = regexp.MustCompile(`^OVERSEER REVIEW ([0-9a-f]{7,64}) (PASS|FAIL)\b`)

// HasOverseerReviewNote reports whether notes carry an "OVERSEER REVIEW <head>
// PASS|FAIL" line naming exactly head. Both verdicts answer a risk-path item:
// a FAIL says the head was reviewed and the overseer files the follow-up,
// which is not the queue's to hold (gt-vsct7.4).
func HasOverseerReviewNote(notes, head string) bool {
	if head == "" {
		return false
	}
	for _, line := range strings.Split(notes, "\n") {
		if m := overseerReviewLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[1] == head {
			return true
		}
	}
	return false
}
