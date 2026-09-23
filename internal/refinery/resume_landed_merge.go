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
//
// Under merge_queue.editorial.required the two shapes of "already landed"
// are handled differently, deliberately (gt-9t0p).
//
// A literal ancestor — an ordinary merge, the identity a merge commit's
// second parent carries forward unchanged — is a landing git itself attests
// to: the commit is on target, so refusing to finish the bookkeeping could
// not un-land it, it would only strand the MR bead and its source issue.
// Bookkeeping completes and a note that cannot be attached is recorded and
// escalated rather than swallowed (failEditorialRecordFailed, gt-wh66
// acceptance criterion 5).
//
// A landing whose sha a rebase or cherry-pick rewrote is attested only by
// patch-id equality of the content, so that patch-id has to be the proof:
// the commit carrying it is resolved and its approve note required, the same
// requirement the push precondition makes of every other landing (T6). When
// that proof cannot be established the MR is NOT completed — the refusal
// leaves it queued and escalates with the command that restores the note.
// Completing it instead would close the MR and its source issue on content
// nobody reviewed, which is the silent route past editorial.required this
// branch used to leave open.
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
		if err := e.ensureLandedEditorialNote(mr, target, mergeCommit); err != nil {
			e.failEditorialRecordFailed("[Engineer]", []*MRInfo{mr}, fmt.Sprintf("resume could not attach an om note to landed commit %s: %v", shortSHA(mergeCommit), err))
		}
	} else {
		landedCommit, refusal := e.requireRenamedLandingProof(mr, target, mergeRef)
		if refusal != nil {
			return *refusal
		}
		mergeCommit = landedCommit
	}

	return ProcessResult{Success: true, MergeCommit: mergeCommit}
}

// requireRenamedLandingProof handles the landing whose sha a rebase or
// cherry-pick rewrote before it reached target: mergeRef itself is not on
// target, only its content is (by patch-id), so the commit that actually
// carries that content has to be resolved before anything can be required of
// it. Returns the commit to report as this MR's merge, or the refusal result
// that must be returned in its place — non-nil means the landing was NOT
// accepted and no bookkeeping may run (gt-9t0p).
//
// On a rig that has not set merge_queue.editorial.required there is no proof
// to require, so the MR is resumed as landed reporting the submitted sha,
// exactly as before this fix. Resolving the landed commit anyway would only
// change the recorded merge_commit, which is not what this fix is about.
func (e *Engineer) requireRenamedLandingProof(mr *MRInfo, target, mergeRef string) (string, *ProcessResult) {
	if e.config.Editorial == nil || !e.config.Editorial.Required {
		return mergeRef, nil
	}

	landedCommit, err := e.findLandedCommitByPatchID(target, mergeRef)
	if err != nil {
		return "", e.refuseResumedLanding(mr, target, editorial.ReasonRangeUnresolvable,
			fmt.Sprintf("could not resolve which commit on %s carries the content of %s: %v", target, shortSHA(mergeRef), err), "")
	}
	if landedCommit == "" {
		return "", e.refuseResumedLanding(mr, target, editorial.ReasonRangeUnresolvable,
			fmt.Sprintf("no first-parent commit on %s carries the content of %s — a landing split across several rewritten commits has no single commit to hang a note on", target, shortSHA(mergeRef)), "")
	}
	if err := e.ensureLandedEditorialNote(mr, target, landedCommit); err != nil {
		return "", e.refuseResumedLanding(mr, target, editorial.ReasonMissing,
			fmt.Sprintf("the commit that landed this MR (%s on %s) has no matching approve note, and none could be backfilled: %v", shortSHA(landedCommit), target, err), landedCommit)
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: landed commit %s on %s identified by patch-id and covered by an approve note\n", mr.ID, shortSHA(landedCommit), target)
	return landedCommit, nil
}

// refuseResumedLanding is the fail-closed tail of the rebase/cherry-pick
// branch (gt-9t0p): the MR's content is on target but carries no approve
// note, so the MR is not treated as landed — no merge_commit is reported, the
// caller's bookkeeping does not run, the MR bead stays open (EditorialRefused
// routes it back to the queue rather than into dead-worker recovery, which
// would re-dispatch reviewed work every cycle), and the witness is told what
// restores the proof.
//
// The failed merge is already on target, so no later merge cycle can produce
// its note: the remedy is a verdict for the diff (gt mq review) plus either an
// audited backfill of that verdict onto the landed commit (gt mq rekey-note,
// which itself refuses unless a matching approve note exists) or, where the
// patch-id proof cannot hold at all (a rebase that resolved conflicts), an
// operator's attested close (gt mq post-merge --landed-commit). Until then
// every cycle reports the same refusal, which is the point — a commit nobody
// reviewed must not close an MR on an editorial.required rig.
func (e *Engineer) refuseResumedLanding(mr *MRInfo, target string, reason editorial.PreconditionReason, detail, landedCommit string) *ProcessResult {
	remedy := fmt.Sprintf("re-review the branch (gt mq review %s) so an approve note covers this diff, then back the note in (gt mq rekey-note %s%s --reason \"...\"), or — for a landing whose patch-id proof cannot hold, e.g. a rebase that resolved conflicts — close it by operator attestation (gt mq post-merge %s %s --landed-commit <sha>)",
		mr.ID, mr.ID, rekeyLandedArg(landedCommit), e.rig.Name, mr.ID)
	cerr := &editorial.PreconditionError{Class: editorial.Precondition, MR: mr.ID, Reason: reason}
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: refusing to complete an already-landed MR — %s\n", mr.ID, detail)
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s stays open in the queue and no bookkeeping ran; %s\n", mr.ID, remedy)
	e.recordEditorialFailure(mr, cerr)
	e.escalateToWitness(fmt.Sprintf("EDITORIAL_RESUME_UNPROVEN: MR %s reason=%s — %s. The content is already on %s, so no merge can write this MR's note: %s",
		mr.ID, reason, detail, target, remedy))
	result := editorialRefusalResult(cerr)
	return &result
}

// rekeyLandedArg names the resolved landed commit in a rekey-note remedy, or
// leaves the sha for the operator to fill in when it could not be resolved
// (`gt mq rekey-note` refuses a sha that is not on target, so a guessed one
// would be worse than a placeholder).
func rekeyLandedArg(landedCommit string) string {
	if strings.TrimSpace(landedCommit) == "" {
		return " --landed <landed-sha>"
	}
	return " --landed " + landedCommit
}

// findLandedCommitByPatchID returns the first-parent commit on origin/target
// that carries mergeRef's content, for a landing mergeRef itself is not
// reachable from (a rebase or cherry-pick rewrote its sha; see
// git.CommitLandedOnTarget's cherry fallback). "" means no such commit —
// the caller must then refuse rather than assume a landing.
//
// The key is the same patch-id a reviewed range's note carries:
// patch-id(merge-base(origin/target, mergeRef)..mergeRef), which is what
// editorial.CheckPrecondition recomputes at push time and what the note
// records (see LandedMR.Base/Head). Candidates are that range's first-parent
// commits on target, newest first, and the first whose own diff has that
// patch-id is the commit this MR's content landed as — a landing merge commit
// qualifies through its diff against its first parent, which is the merged
// branch's cumulative diff. This is what lets the proof be checked keyed by
// patch-id when the sha changed, instead of by a sha that no longer exists.
func (e *Engineer) findLandedCommitByPatchID(target, mergeRef string) (string, error) {
	targetRef := "origin/" + target
	base, err := e.git.MergeBase(targetRef, mergeRef)
	if err != nil {
		return "", fmt.Errorf("merge-base of %s and %s: %w", targetRef, shortSHA(mergeRef), err)
	}
	if base == mergeRef {
		// mergeRef is the merge base itself: there is no range of its own
		// whose patch-id could be looked for.
		return "", nil
	}
	want, err := e.git.PatchID(base, mergeRef)
	if err != nil {
		return "", fmt.Errorf("patch-id of %s..%s: %w", shortSHA(base), shortSHA(mergeRef), err)
	}
	pairs, err := e.git.FirstParentPatchIDs(base, targetRef)
	if err != nil {
		return "", fmt.Errorf("first-parent patch-ids of %s..%s: %w", base, targetRef, err)
	}
	for _, p := range pairs {
		if p.PatchID == want {
			return p.Commit, nil
		}
	}
	return "", nil
}

// ensureLandedEditorialNote ensures landedCommit — the commit that actually
// carried mergeRef's content onto target, as resumeLandedMerge resolved it —
// carries a matching om approve note, for an MR resumeLandedMerge is
// completing after the fact. Returns nil when it does (including when the rig
// has not set merge_queue.editorial.required, where there is nothing to
// require), or an error explaining why the proof could not be established.
//
// When landedCommit has no matching note, this backfills it from an approve
// note whose patch-id matches the landed diff, via editorial.RekeyNote (which
// independently re-verifies the patch-id before writing anything). RekeyNote
// is asked with AllowAnyMR: true — the source note does not have to belong to
// mr.ID (gt-bagu).
//
// This matters because the review step that produced the note does not scope
// its reuse lookup by MR either: gt mq review answers a diff from any note in
// refs/notes/om carrying the same patch-id, whatever MR wrote it
// (FindVerdictForDiff, gt-qa2p — deliberate, since MR beads are wisps that
// are routinely reaped and re-minted for the same diff). editorial_reviewed_
// head can therefore legitimately name a commit whose note carries an older
// or unrelated MR id. Requiring an exact MR match here — the shape gt-9t0p
// originally shipped — refuses that landing every cycle with no path to a
// verdict it will ever accept: the note that proves the diff exists and is
// readable, but resumeLandedMerge and gt mq review disagree about whether an
// MR id has to match it. Patch-id equality is what both CheckPrecondition and
// FindVerdictForDiff already treat as proof; requiring MR-id equality on top
// of it, only here, is what actually looped this MR (gt-bagu), not any
// ancestor relationship between the reviewed head and the landing.
func (e *Engineer) ensureLandedEditorialNote(mr *MRInfo, target, landedCommit string) error {
	if e.config.Editorial == nil || !e.config.Editorial.Required {
		return nil
	}
	if strings.TrimSpace(landedCommit) == "" {
		return nil
	}

	if covered, checkErr := e.landedCommitHasApproveNote(landedCommit); checkErr == nil && covered {
		return nil
	}

	result, err := editorial.RekeyNote(e.git, editorial.RekeyRequest{
		MR:         mr.ID,
		Landed:     landedCommit,
		Target:     target,
		AllowAnyMR: true,
		Reason:     "gt-wh66/gt-bagu: automatic resume after an interrupted merge — refinery died between push and bookkeeping; backfilling the approve note that covers this diff by patch-id onto the commit that already landed",
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Backfilled om note onto resumed landed commit %s (patch-id %s)\n", shortSHA(landedCommit), result.PatchID)
	return nil
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
