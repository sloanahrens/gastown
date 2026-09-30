package refinery

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/land"
)

// This file refuses to gate or land an MR whose merge into its target changes
// nothing (gt-j5cc). No other stage sees that: the container suite tests the
// target's own tree and passes, docs-lint passes, and the queue reports the MR
// "ready" — so a conflict-resolution commit that deleted a branch's entire
// payload reached the push step, where merging it produced a tree
// byte-identical to the target and would have closed the source issue as
// merged with its bug still live.
//
// The predicate is tree equality, which is what an empty `git diff --stat
// <target> <branch-head>` means. It runs at three points: in doMerge before the
// gates, against the submitted head; in doMerge after the local merge, against
// the merge result; and in BuildRebaseStack after each member's merge, against
// the stack. The second point is not a repeat of the first — a branch whose
// patch the target already carries has a tree of its own that differs from the
// target's, so only the merge result can show that merging it changes nothing —
// and the third exists because a batch pushes and closes every member it
// stacked, so an empty member left in the stack is the same bug on another
// path.

// emptyMerge is one refusal's evidence; the type and its reason text moved
// to land, which refuses empty merges too (gt-v4ssj.9).
type emptyMerge = land.EmptyMerge

// checkSubmittedHeadAddsChange refuses mr when merging head into target would
// leave target's tree as it is.
//
// It measures against origin/target rather than the local target ref, which the
// merge no longer stages on and which a live polecat worktree can leave
// arbitrarily stale (gt-032w).
func (e *Engineer) checkSubmittedHeadAddsChange(mr *MRInfo, target, head string) ProcessResult {
	if e.git == nil {
		return ProcessResult{Success: false, Error: "git client is missing"}
	}
	base := "origin/" + target
	identical, err := e.git.TreesIdentical(base, head)
	if err != nil {
		return ProcessResult{Success: false, Error: fmt.Sprintf("comparing %s with %s: %v", base, shortSHA(head), err)}
	}
	if !identical {
		return ProcessResult{Success: true}
	}
	return e.refuseEmptyMerge(mr, emptyMerge{
		Target:     target,
		Base:       base,
		Head:       head,
		Stage:      "before gates",
		Comparison: fmt.Sprintf("%s and %s have identical trees", base, shortSHA(head)),
	})
}

// checkLandedMergeAddsChange refuses mr when the checked-out merge of head into
// target adds nothing to what origin/target already holds. It runs with the
// merge commit at HEAD, and the caller resets target afterwards.
func (e *Engineer) checkLandedMergeAddsChange(mr *MRInfo, target, head string) ProcessResult {
	if e.git == nil {
		return ProcessResult{Success: false, Error: "git client is missing"}
	}
	identical, err := e.git.TreesIdentical("origin/"+target, "HEAD")
	if err != nil {
		return ProcessResult{Success: false, Error: fmt.Sprintf("comparing origin/%s with the merge result: %v", target, err)}
	}
	if !identical {
		return ProcessResult{Success: true}
	}
	return e.refuseEmptyMerge(mr, emptyMerge{
		Target:     target,
		Base:       "origin/" + target,
		Head:       head,
		Stage:      "before push",
		Comparison: fmt.Sprintf("the merge result and origin/%s have identical trees", target),
	})
}

// refuseEmptyMerge closes mr as ineligible with the evidence, so the queue
// cannot retry a branch that will never add anything and a reviewer can read
// which commit did the damage. It closes here for the same reason
// recheckMRStillMergeable does: a caller that inspects a NoMerge result is not
// guaranteed to dequeue it, and an MR left open re-gates an unchanged branch
// forever. A caller that does dequeue it, like HandleMRInfoFailure, finds the
// bead already closed and skips the close.
//
// The source issue is left alone: an empty MR reports nothing about whether the
// work was ever done.
//
// The bead is left to the caller under testAllowSyntheticMRs, like every other
// beads write on the merge-mechanics path.
func (e *Engineer) refuseEmptyMerge(mr *MRInfo, ev emptyMerge) ProcessResult {
	reason := e.emptyMergeReason(ev)
	if !e.isSyntheticMergeMechanicsMR(mr) {
		if closeErr := e.closeIneligibleMR(mr, reason); closeErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close empty MR %s: %v\n", mr.ID, closeErr)
		}
	}
	return mergeIneligibleResult("%s", reason)
}

// emptyMergeReason builds the refusal text (moved to land.EmptyMergeReason).
func (e *Engineer) emptyMergeReason(ev emptyMerge) string {
	return land.EmptyMergeReason(e.git, ev)
}
