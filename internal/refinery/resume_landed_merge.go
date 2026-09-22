package refinery

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// This file recognizes an MR that already landed on its target — a prior
// refinery pass merged and pushed it but died before finishing bookkeeping —
// and resumes instead of re-gating or re-merging it (gt-wh66). Nothing else
// in doMerge sees that: the merge/push half of a land completed, but the MR
// bead is still 'open' because the bookkeeping half (close the MR, close the
// source issue, delete the branch, copy the om note) runs afterward as
// separate, non-resumable steps. A crash between the two leaves an MR that
// `gt mq list` reports as 'ready' and the queue re-gates on the next cycle,
// wasting the suite on a diff already proven — and, under
// merge_queue.editorial.required, can leave the landed commit with no om
// note and nobody told.

// mergeAlreadyLanded reports whether mergeRef — the submitted branch's live
// head, already resolved and validated by submittedBranchHead — was already
// pushed to origin/target by an earlier, real merge (git.CommitLandedOnTarget;
// see its doc comment for why this is stricter than treating any no-op merge
// as landed, and what it cannot detect).
func (e *Engineer) mergeAlreadyLanded(target, mergeRef string) bool {
	if e.git == nil {
		return false
	}
	return e.git.CommitLandedOnTarget("origin", target, mergeRef)
}

// resumeLandedMerge completes an MR whose submitted commit already landed on
// target (mergeAlreadyLanded). It reconciles the om note when editorial
// review is required and returns success so the caller's ordinary
// HandleMRInfoSuccess runs unchanged: closes the MR bead, closes the source
// issue, deletes the branch, and nudges the mayor exactly as it would for a
// merge doMerge just performed itself. No gate, conflict check, or push runs
// here — the whole point is that a prior pass already did that part.
func (e *Engineer) resumeLandedMerge(mr *MRInfo, target, mergeRef string) ProcessResult {
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: submitted commit %s already reachable from %s — resuming post-merge bookkeeping instead of re-merging\n",
		mr.ID, shortSHA(mergeRef), target)

	// mergeAlreadyLanded can fire two ways: mergeRef is a literal ancestor of
	// target (an ordinary merge — the identity a merge commit's second parent
	// carries forward unchanged), or a patch-id-preserving rebase rewrote its
	// SHA entirely (git.CommitLandedOnTarget's cherry fallback). Only the
	// first case has a specific landed commit findMergeCommitFor can locate;
	// walking origin/target's history from a mergeRef that never actually
	// appears in it would search a meaningless range.
	ancestor, ancestorErr := e.git.IsAncestor(mergeRef, "origin/"+target)
	if ancestorErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not confirm %s's ancestry on %s: %v\n", shortSHA(mergeRef), target, ancestorErr)
	}

	mergeCommit := mergeRef
	if ancestor {
		// Best-effort, independent of editorial: knowing the actual merge
		// commit (rather than just the submitted branch tip) makes the
		// bead's recorded merge_commit accurate, the same value
		// HandleMRInfoSuccess would record had this pass performed the merge
		// itself. "" (fast-forward, or the walk failed) keeps mergeRef.
		landedCommit, err := e.findMergeCommitFor(target, mergeRef)
		if err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not locate the merge commit that landed %s on %s: %v\n", shortSHA(mergeRef), target, err)
		}
		if landedCommit != "" {
			mergeCommit = landedCommit
		}
		e.reconcileLandedEditorialNote(mr, target, mergeCommit)
	} else if e.config.Editorial != nil && e.config.Editorial.Required {
		// A rebase changed mergeRef's SHA before landing, so there is no
		// commit here to key an automatic backfill to: RekeyNote requires
		// its Landed argument to actually be reachable from target, and
		// mergeRef itself is not (only its content is, by patch-id). Rather
		// than guess at (or fail loudly on) the wrong SHA, defer to the
		// operator with the exact command that can name the real one.
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s landed via a content-preserving rebase; the specific landed commit could not be identified automatically — if it is missing its om note, back it in with: gt mq rekey-note %s --landed <landed-sha> --reason \"...\"\n", mr.ID, mr.ID)
	}

	return ProcessResult{Success: true, MergeCommit: mergeCommit}
}

// reconcileLandedEditorialNote ensures landedCommit — the commit that
// actually carried mergeRef's content onto target, as resumeLandedMerge
// resolved it — carries a matching om approve note, for an MR
// resumeLandedMerge is completing after the fact. No-op when the rig has not
// set merge_queue.editorial.required.
//
// When landedCommit has no matching note, this backfills it from the
// pre-push approve note this MR must have carried (the same one
// editorialPrecondition required before the original push) via
// editorial.RekeyNote, which independently verifies the patch-id still
// matches before writing anything. When no matching note can be found, or the
// landed diff's patch-id no longer matches the note that exists, this is
// treated exactly like a copyEditorialNotes failure: a verdict-without-proof
// on a commit `git log` already shows, recorded and escalated to the witness
// rather than left silent (acceptance criterion 5).
func (e *Engineer) reconcileLandedEditorialNote(mr *MRInfo, target, landedCommit string) {
	if e.config.Editorial == nil || !e.config.Editorial.Required {
		return
	}
	if strings.TrimSpace(landedCommit) == "" {
		return
	}

	if covered, checkErr := e.landedCommitHasApproveNote(landedCommit); checkErr == nil && covered {
		return
	}

	result, err := editorial.RekeyNote(e.git, editorial.RekeyRequest{
		MR:     mr.ID,
		Landed: landedCommit,
		Target: target,
		Reason: "gt-wh66: automatic resume after an interrupted merge — refinery died between push and bookkeeping; backfilling the pre-push approve note onto the commit that already landed",
	})
	if err != nil {
		e.failEditorialRecordFailed("[Engineer]", []*MRInfo{mr}, fmt.Sprintf("resume could not attach an om note to landed commit %s: %v", shortSHA(landedCommit), err))
		return
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Backfilled om note onto resumed landed commit %s (patch-id %s)\n", shortSHA(landedCommit), result.PatchID)
}

// findMergeCommitFor returns the two-parent merge commit reachable from
// origin/target whose second parent is exactly mergeRef — the commit that
// brought mergeRef onto target via a plain (non-squash) merge — or "" when
// none is found (a fast-forward landing has no such commit).
//
// Callers must confirm mergeRef is an ancestor of origin/target first:
// FirstParentLog(mergeRef, "origin/"+target) only searches a meaningful
// range when that holds (merge-base(mergeRef, origin/target) is then
// mergeRef itself, so the range covers everything landed since, not just
// this MR's own merge) — resumeLandedMerge is the only caller and checks
// this before calling in.
func (e *Engineer) findMergeCommitFor(target, mergeRef string) (string, error) {
	commits, err := e.git.FirstParentLog(mergeRef, "origin/"+target)
	if err != nil {
		return "", err
	}
	for _, c := range commits {
		parents, perr := e.git.Parents(c)
		if perr != nil {
			continue
		}
		if len(parents) >= 2 && parents[1] == mergeRef {
			return c, nil
		}
	}
	return "", nil
}

// landedCommitHasApproveNote reports whether commit already carries an
// approve note whose patch-id matches its own diff from its first parent —
// the same test the editorial-coverage doctor check applies.
func (e *Engineer) landedCommitHasApproveNote(commit string) (bool, error) {
	note, err := editorial.ReadNote(e.git, commit)
	if err != nil {
		if err == git.ErrNoNote {
			return false, nil
		}
		return false, err
	}
	if note.Verdict != "approve" {
		return false, nil
	}
	parents, err := e.git.Parents(commit)
	if err != nil || len(parents) == 0 {
		return false, err
	}
	patchID, err := e.git.PatchID(parents[0], commit)
	if err != nil {
		return false, err
	}
	return note.PatchID == patchID, nil
}
