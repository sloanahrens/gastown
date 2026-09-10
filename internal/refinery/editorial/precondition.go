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
type LandedMR struct {
	MRID         string
	ReviewedHead string
	Base         string
	Head         string
}

// ClassifiedError is a push-precondition failure, classified so callers
// route on Class/Reason rather than message text (see the fail-closed
// table in the design spec).
type ClassifiedError struct {
	Class  FailureClass
	MR     string
	Reason string // missing | patch_id_mismatch | version_below_min | verdict_not_approve
}

func (e *ClassifiedError) Error() string {
	return fmt.Sprintf("editorial precondition failed for %s: %s", e.MR, e.Reason)
}

// CheckPrecondition verifies, for every MR about to land, that an approve
// note exists on its reviewed head, that the note's patch-id still matches
// the MR's range as it sits on the branch about to be pushed, and that the
// reviewing om version meets cfg.MinVersion. It returns the notes read (one
// per mr, same order) so the caller can copy them onto the landed commits
// without re-reading. Skipped entirely when cfg.Required is false —
// upstream (pre-gate) behavior for rigs that never set the editorial gate.
func CheckPrecondition(g *git.Git, cfg config.EditorialConfig, mrs []LandedMR) ([]Note, *ClassifiedError) {
	if !cfg.Required {
		return nil, nil
	}

	notes := make([]Note, 0, len(mrs))
	for _, mr := range mrs {
		note, err := readReviewedNote(g, mr.ReviewedHead)
		if err != nil {
			return nil, &ClassifiedError{Class: Precondition, MR: mr.MRID, Reason: "missing"}
		}

		patchID, err := g.PatchID(mr.Base, mr.Head)
		if err != nil {
			return nil, &ClassifiedError{Class: Precondition, MR: mr.MRID, Reason: fmt.Sprintf("patch-id: %v", err)}
		}
		if patchID != note.PatchID {
			return nil, &ClassifiedError{Class: Precondition, MR: mr.MRID, Reason: "patch_id_mismatch"}
		}

		if cfg.MinVersion != "" && deps.CompareVersions(note.OMVersion, cfg.MinVersion) < 0 {
			return nil, &ClassifiedError{Class: Precondition, MR: mr.MRID, Reason: "version_below_min"}
		}

		if note.Verdict != "approve" {
			return nil, &ClassifiedError{Class: Precondition, MR: mr.MRID, Reason: "verdict_not_approve"}
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

// CopyNotesToLanded copies each note from its reviewed head onto the
// landed commit, when they differ — a stacked or rebased merge changes the
// commit identity even though the patch-id (and so the note's proof)
// still applies. A no-op per MR when ReviewedHead == Head, e.g. the
// single-MR path where the two are the same commit.
func CopyNotesToLanded(g *git.Git, mrs []LandedMR, notes []Note) error {
	if len(mrs) != len(notes) {
		return fmt.Errorf("CopyNotesToLanded: %d MRs but %d notes", len(mrs), len(notes))
	}
	for _, mr := range mrs {
		if mr.ReviewedHead == "" || mr.ReviewedHead == mr.Head {
			continue
		}
		if err := g.NotesCopy(NotesRef, mr.ReviewedHead, mr.Head); err != nil {
			return fmt.Errorf("copy note for %s: %w", mr.MRID, err)
		}
	}
	return nil
}
