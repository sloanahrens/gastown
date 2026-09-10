package editorial

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/git"
)

// LandedMR describes one MR about to land: its bead id, the head commit
// that gt mq review reviewed (from editorial_reviewed_head on the MR
// bead), and the range (Base, Head) as it sits on the branch about to be
// pushed. Base/Head are recomputed by the caller at push time — never read
// from the note — so a rebase or conflict resolution that changes the diff
// produces a different patch-id and the precondition catches it.
//
// LandedCommit is the actual commit that lands on target (the MergeNoFF
// merge commit, or the fast-forward stack member's own commit) — distinct
// from Head, the submitted branch tip Base/Head range checking is anchored
// to. The two coincide only when the merge is itself a fast-forward of
// Head. CopyNotesToLanded targets LandedCommit, not Head, so the note
// ends up on the commit `git log` on target actually shows — the caller
// sets it once the real landed SHA is known, after the merge/push
// completes.
type LandedMR struct {
	MRID         string
	ReviewedHead string
	Base         string
	Head         string
	LandedCommit string
}

// PreconditionReason enumerates the push-precondition's classified
// failure reasons. Callers route on this (and Class), never on Error()'s
// message text (see the fail-closed table in the design spec).
type PreconditionReason string

const (
	ReasonMissing           PreconditionReason = "missing"
	ReasonPatchIDMismatch   PreconditionReason = "patch_id_mismatch"
	ReasonVersionBelowMin   PreconditionReason = "version_below_min"
	ReasonVerdictNotApprove PreconditionReason = "verdict_not_approve"
	ReasonRangeUnresolvable PreconditionReason = "range_unresolvable"
)

// PreconditionError is a push-precondition failure, classified so callers
// route on Class/Reason rather than message text (see the fail-closed
// table in the design spec). Named distinctly from ClassifiedError
// (classified_error.go) — that type pairs a FailureClass with an
// underlying error for review-pipeline failures; this one carries the MR
// id and an enumerated Reason for push-precondition failures instead.
type PreconditionError struct {
	Class  FailureClass
	MR     string
	Reason PreconditionReason
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("editorial precondition failed for %s: %s", e.MR, e.Reason)
}

// CheckPrecondition verifies, for every MR about to land, that an approve
// note exists on its reviewed head, that the reviewing om version meets
// cfg.MinVersion, and that the note's patch-id still matches the MR's range
// as it sits on the branch about to be pushed. It returns the notes read
// (one per mr, same order) so the caller can copy them onto the landed
// commits without re-reading. Skipped entirely when cfg.Required is false —
// upstream (pre-gate) behavior for rigs that never set the editorial gate.
//
// Checks run cheapest-and-most-informative first: verdict, then version,
// then patch-id. A request_changes note whose range was later edited (or
// that predates cfg.MinVersion) must report verdict_not_approve, not
// patch_id_mismatch or version_below_min — checking patch-id or version
// first would name the wrong reason on a note that was already refusing
// for an unrelated cause.
func CheckPrecondition(g *git.Git, cfg config.EditorialConfig, mrs []LandedMR) ([]Note, *PreconditionError) {
	if !cfg.Required {
		return nil, nil
	}

	notes := make([]Note, 0, len(mrs))
	for _, mr := range mrs {
		note, err := readReviewedNote(g, mr.ReviewedHead)
		if err != nil {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonMissing}
		}

		if note.Verdict != "approve" {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonVerdictNotApprove}
		}

		// Empty or "dev" (the unset-ldflags default) is treated as below any
		// floor, matching AssertVersion — CompareVersions would otherwise
		// silently map an empty string to 0.0.0.
		if cfg.MinVersion != "" && (note.OMVersion == "" || note.OMVersion == "dev" || deps.CompareVersions(note.OMVersion, cfg.MinVersion) < 0) {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonVersionBelowMin}
		}

		patchID, err := g.PatchID(mr.Base, mr.Head)
		if err != nil {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonRangeUnresolvable}
		}
		if patchID != note.PatchID {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonPatchIDMismatch}
		}

		notes = append(notes, *note)
	}
	return notes, nil
}

// readReviewedNote reads the note on reviewedHead, treating an empty
// reviewedHead (never reviewed — editorial_reviewed_head was never
// written) the same as git.ErrNoNote.
func readReviewedNote(g *git.Git, reviewedHead string) (*Note, error) {
	if reviewedHead == "" {
		return nil, git.ErrNoNote
	}
	return ReadNote(g, reviewedHead)
}

// CopyNotesToLanded copies each note from its reviewed head onto
// LandedCommit (the actual commit that lands on target — see LandedMR),
// when they differ — a stacked or rebased merge changes the commit
// identity even though the patch-id (and so the note's proof) still
// applies. A no-op per MR when ReviewedHead == LandedCommit, e.g. a
// fast-forward where the two are the same commit.
func CopyNotesToLanded(g *git.Git, mrs []LandedMR, notes []Note) error {
	if len(mrs) != len(notes) {
		return fmt.Errorf("CopyNotesToLanded: %d MRs but %d notes", len(mrs), len(notes))
	}
	for _, mr := range mrs {
		landed := mr.LandedCommit
		if landed == "" {
			landed = mr.Head
		}
		if mr.ReviewedHead == "" || mr.ReviewedHead == landed {
			continue
		}
		if err := g.NotesCopy(NotesRef, mr.ReviewedHead, landed); err != nil {
			return fmt.Errorf("copy note for %s: %w", mr.MRID, err)
		}
	}
	return nil
}
