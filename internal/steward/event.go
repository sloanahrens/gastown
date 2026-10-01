package steward

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// Kind is the landing-queue event a job answers. The set is closed: these are
// the two events the daemon watches for (gt-9bioi).
type Kind string

const (
	// KindReview: a bead gained gt:ready-to-land with a head no job has seen.
	KindReview Kind = "review"
	// KindRejection: the landing worker rejected a bead, which now carries
	// the rework label and a MERGE REJECTION block naming the rejected head.
	KindRejection Kind = "rejection"
)

// Event is one landing-queue event, carrying everything a job's prompt needs
// so the job reads the bead for judgement, not for facts (gt-9bioi.2).
type Event struct {
	Kind   Kind   `json:"kind"`
	Rig    string `json:"rig"`
	Bead   string `json:"bead"`
	Branch string `json:"branch,omitempty"`
	// Head is the submitted (review) or rejected (rejection) tip.
	Head   string `json:"head,omitempty"`
	Target string `json:"target,omitempty"`
	// Worker is the polecat whose work this is, from the READY TO LAND block.
	Worker string `json:"worker,omitempty"`
	// Attempt is how many rejections the bead carries; 0 for a review.
	Attempt int `json:"attempt,omitempty"`
	// RejectionDetail is what the landing worker refused, empty for a review.
	RejectionDetail string `json:"rejection_detail,omitempty"`
	// Mode is the mode the job will run in, set by the daemon after Detect.
	Mode Mode `json:"mode,omitempty"`
}

// Key is the dedupe key: one event on one head of one bead. A resubmission
// pushes a new head, so a fixed-then-rejected bead earns a new job
// (gt-9bioi.1).
func (e Event) Key() string {
	return strings.Join([]string{string(e.Kind), e.Bead, e.Head}, "|")
}

// CrewAssigned reports whether assignee names a crew member. Crew work is
// reviewed by its owner: a steward job on it would race the human who is
// already looking at the branch (gt-9bioi).
func CrewAssigned(assignee string) bool {
	return strings.Contains(assignee, "/crew/")
}

// Detect returns the event issue raises, if any. seen reports whether a job
// already ran for an event key, so a head a job has handled is not re-run
// (the caller passes the ledger's answer).
//
// Order matters: a bead the landing worker rejected carries the rework label
// and no ready label, since Land swaps them. A bead carrying both is
// mid-transition; the ready label wins, because the landing worker is about
// to judge it anyway.
func Detect(issue *beads.Issue, rig string, seen func(key string) bool) (Event, bool) {
	if issue == nil || CrewAssigned(issue.Assignee) {
		return Event{}, false
	}
	// A bead only a human can unblock is the operator's, not a job's.
	if beads.HasLabel(issue, land.LabelNeedsHuman) {
		return Event{}, false
	}
	if beads.HasLabel(issue, land.LabelReadyToLand) {
		w, err := land.WorkFromBead(issue, rig)
		if err != nil {
			return Event{}, false
		}
		ev := Event{Kind: KindReview, Rig: rig, Bead: issue.ID, Branch: w.Branch, Head: w.Head, Target: w.Target, Worker: w.Worker}
		if seen(ev.Key()) {
			return Event{}, false
		}
		return ev, true
	}
	if beads.HasLabel(issue, land.LabelRework) {
		note, ok := land.ParseRejectionNote(issue.Notes)
		if !ok || note.Head == "" {
			return Event{}, false
		}
		ev := Event{
			Kind: KindRejection, Rig: rig, Bead: issue.ID,
			Branch: note.Branch, Head: note.Head, Target: note.Target,
			Attempt:         note.Attempt,
			RejectionDetail: RejectionDetail(note),
		}
		if seen(ev.Key()) {
			return Event{}, false
		}
		return ev, true
	}
	return Event{}, false
}

// RejectionDetail renders the rejection a job must act on as one line: the
// class, the reason, the conflicting files and om's findings. The gate tail
// stays in the bead's notes, where the job reads it whole.
func RejectionDetail(note land.RejectionNote) string {
	parts := make([]string, 0, 4)
	if note.Kind != "" {
		parts = append(parts, "kind="+note.Kind)
	}
	if note.Reason != "" {
		parts = append(parts, "reason="+land.NoteField(note.Reason))
	}
	if len(note.Conflicting) > 0 {
		files := make([]string, len(note.Conflicting))
		for i, f := range note.Conflicting {
			files[i] = land.NoteField(f)
		}
		parts = append(parts, "conflicting="+strings.Join(files, ","))
	}
	if len(note.Findings) > 0 {
		findings := make([]string, len(note.Findings))
		for i, f := range note.Findings {
			findings[i] = f.ID + ":" + f.Path
		}
		parts = append(parts, "findings="+strings.Join(findings, ","))
	}
	return strings.Join(parts, " ")
}
