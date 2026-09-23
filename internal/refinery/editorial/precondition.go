package editorial

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// LandedMR describes one MR about to land: its bead id, the head commit
// that gt mq review reviewed (from editorial_reviewed_head on the MR
// bead), and the range (Base, Head) as it sits on the branch about to be
// pushed. Base/Head are recomputed by the caller at push time — never read
// from the note — so a rebase or conflict resolution that changes the diff
// produces a different patch-id and the precondition catches it.
//
// TargetTip is target's own tip — origin/<target> — resolved by the caller
// at the same push-time moment as Base/Head, mirroring Note.ReviewedTargetTip.
// Base alone cannot tell whether target has moved since review: a branch cut
// once and never rebased keeps the same merge-base no matter how far target
// advances past it, so Base is invariant to exactly the movement this field
// exists to detect (gt-6bsp).
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
	TargetTip    string
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
	// ReasonTargetDriftMaterial means target has moved onto files this MR's
	// own diff also touches since the note was reviewed (see
	// targetDriftedMaterially): the MR's own diff is still byte-identical to
	// what was reviewed — patch-id above already proved that — but nobody has
	// ever reviewed it combined with what target gained meanwhile. Not a
	// verdict about the diff (like ReasonVerdictNotApprove), so the MR stays
	// queued rather than closed: the next cycle's ordinary review path stamps
	// a fresh note against the new base, same as ReasonPatchIDMismatch.
	ReasonTargetDriftMaterial PreconditionReason = "target_drift_material"
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
// cfg.MinVersion, that the note's patch-id still matches the MR's range as
// it sits on the branch about to be pushed, and that target has not moved
// onto files that range also touches since the note was written. It returns
// the notes read (one per mr, same order) so the caller can copy them onto
// the landed commits without re-reading. Skipped entirely when cfg.Required
// is false — upstream (pre-gate) behavior for rigs that never set the
// editorial gate.
//
// Checks run cheapest-and-most-informative first: verdict, then version,
// then patch-id, then target drift. A request_changes note whose range was
// later edited (or that predates cfg.MinVersion) must report
// verdict_not_approve, not patch_id_mismatch or version_below_min —
// checking patch-id or version first would name the wrong reason on a note
// that was already refusing for an unrelated cause. Drift runs last and
// only after patch-id has already matched: it is the priciest check (two
// extra diffs) and only meaningful once the MR's own diff is confirmed
// unchanged from what was reviewed.
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

		if omVersionBelowFloor(note.OMVersion, cfg.MinVersion) {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonVersionBelowMin}
		}

		patchID, err := g.PatchID(mr.Base, mr.Head)
		if err != nil {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonRangeUnresolvable}
		}
		if patchID != note.PatchID {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonPatchIDMismatch}
		}

		drifted, err := targetDriftedMaterially(g, note.ReviewedTargetTip, mr.TargetTip, mr.Base, mr.Head)
		if err != nil {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonRangeUnresolvable}
		}
		if drifted {
			return nil, &PreconditionError{Class: Precondition, MR: mr.MRID, Reason: ReasonTargetDriftMaterial}
		}

		notes = append(notes, *note)
	}
	return notes, nil
}

// targetDriftedMaterially reports whether target has moved onto files this
// MR's own diff also touches since reviewedTip (target's tip when the note
// was written) — the gap gt-6bsp exists to close: a clean, non-conflicting
// landing onto a newer target can still combine two independently-approved
// changes that neither review ever saw together. The patch-id check above
// only proves the MR's own diff is byte-identical to what was reviewed; it
// says nothing about what target gained meanwhile, because Base (the
// merge-base) does not move as target advances past it.
//
// Either tip being empty means "unknown" (a note written before this field
// existed, or a landed review with no "since review" window to measure) —
// skipped rather than guessed, matching every other field this codebase
// added after the fact. No movement (reviewedTip == currentTip, e.g. this
// MR lands before anything else does) costs nothing beyond the string
// comparison — the two extra diffs below run only once target has actually
// moved, and never invoke om a second time: this only ever refuses a push,
// leaving the next cycle's ordinary review path to stamp a fresh note
// against the new base.
func targetDriftedMaterially(g *git.Git, reviewedTip, currentTip, base, head string) (bool, error) {
	if reviewedTip == "" || currentTip == "" || reviewedTip == currentTip {
		return false, nil
	}
	landedOnTarget, err := g.DiffNameOnly(reviewedTip, currentTip)
	if err != nil {
		return false, fmt.Errorf("diff target movement %s..%s: %w", reviewedTip, currentTip, err)
	}
	if len(landedOnTarget) == 0 {
		return false, nil
	}
	ownDiff, err := g.DiffNameOnly(base, head)
	if err != nil {
		return false, fmt.Errorf("diff MR's own range %s..%s: %w", base, head, err)
	}
	return pathsOverlap(landedOnTarget, ownDiff), nil
}

// pathsOverlap reports whether a and b share any path.
func pathsOverlap(a, b []string) bool {
	set := make(map[string]bool, len(a))
	for _, p := range a {
		set[p] = true
	}
	for _, p := range b {
		if set[p] {
			return true
		}
	}
	return false
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
