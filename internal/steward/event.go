package steward

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// Kind is the event a job answers. The set is closed (gt-9bioi).
type Kind string

const (
	// KindReview: a bead gained gt:ready-to-land with a head no job has seen.
	KindReview Kind = "review"
	// KindRejection: the landing worker rejected a bead, which now carries
	// the rework label and a MERGE REJECTION block naming the rejected head.
	KindRejection Kind = "rejection"
	// KindPlan: an open spec carries the needs-planning label and no plan
	// proposal yet, so the planner is asked to break it down. It has no head
	// and no branch: the spec is a bead to decompose, not a submission to
	// judge.
	//
	// It is not a patrols.steward.kinds value, and the scan does not raise it
	// yet: planning is its own default-off flag, so a town that scans the
	// landing queue is not also asking for planning work (gt-4k3fj.13,
	// scheduled by gt-4k3fj.14).
	KindPlan Kind = "plan"
)

// ParseKind reads one entry of patrols.steward.kinds: the landing-queue kinds
// the scan may cover. The set is closed: an empty string and anything that is
// not one of them are errors naming the key (gt-9bioi.7). KindPlan is not one
// of them — planning has its own flag (gt-4k3fj.13).
func ParseKind(s string) (Kind, error) {
	switch Kind(s) {
	case KindReview:
		return KindReview, nil
	case KindRejection:
		return KindRejection, nil
	}
	return "", fmt.Errorf("patrols.steward.kinds %q is neither %q nor %q", s, KindReview, KindRejection)
}

// DefaultKinds is what a scan covers when patrols.steward.kinds is absent:
// rejections only. A review job repeats what om (which gates every landing)
// and the overseer's risk review already decide, so it spends LLM calls for
// no decision and the operator opts into it (gt-9bioi.7).
func DefaultKinds() []Kind { return []Kind{KindRejection} }

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
// (gt-9bioi.1). A plan event names no head, so its key is one event on one
// bead (gt-4k3fj.13).
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
// to judge it anyway. A plan event comes last of all: a bead that is both a
// spec and submitted work is a submission, which the landing worker is about
// to judge, and a plan proposal on it would be written over a queue entry.
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
	return planEvent(issue, rig, seen)
}

// planEvent is the event an open needs-planning spec raises: no planner has
// proposed a breakdown for it yet. A spec whose notes carry a proposal is
// planned — the proposal is the deliverable, and the operator deletes the
// block to ask for another (gt-4k3fj.13).
func planEvent(issue *beads.Issue, rig string, seen func(key string) bool) (Event, bool) {
	if issue.Status != string(beads.StatusOpen) || !beads.HasLabel(issue, specdispatch.NeedsPlanningLabel) {
		return Event{}, false
	}
	if HasPlanProposal(issue.Notes) {
		return Event{}, false
	}
	ev := Event{Kind: KindPlan, Rig: rig, Bead: issue.ID}
	if seen(ev.Key()) {
		return Event{}, false
	}
	return ev, true
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
