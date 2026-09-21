package refinery

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
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

const (
	// emptyMergeScanLimit bounds how many of the branch's own commits the
	// refusal inspects. The commit that lost the payload is always one of the
	// most recent that remove content, so a window from the tip is enough.
	emptyMergeScanLimit = 20

	// emptyMergeReportLimit caps how many commits the refusal lists before it
	// counts the rest.
	emptyMergeReportLimit = 8
)

// emptyMerge is one refusal's evidence.
type emptyMerge struct {
	// Target is the branch being merged into, for the message.
	Target string
	// Base is the ref the branch's own commits are measured against. It must
	// be one the merge has not moved: after the local merge, refs/heads/target
	// contains the merge commit and reachability from it hides the branch's
	// whole history (gt-j5cc).
	Base string
	// Head is the submitted branch head, whose commits are blamed.
	Head string
	// Stage says where the check ran, which is what separates a branch that
	// was already empty when submitted from a merge that turned out to be
	// empty.
	Stage string
	// Comparison names the two refs found to hold identical trees.
	Comparison string
}

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

// emptyMergeReason builds the refusal text. Its first line stands alone,
// because that is the part a close reason or a log line keeps.
func (e *Engineer) emptyMergeReason(ev emptyMerge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "empty merge (%s): %s, so this MR changes nothing in %s",
		ev.Stage, ev.Comparison, ev.Target)

	commits, err := e.git.CommitLineStatsInRange(ev.Base+".."+ev.Head, emptyMergeScanLimit)
	if err != nil {
		fmt.Fprintf(&b, " (the branch's own commits could not be read: %v)", err)
		return b.String()
	}
	if len(commits) == 0 {
		fmt.Fprintf(&b, "; the branch has no commits %s does not already have", ev.Base)
		return b.String()
	}

	b.WriteString("; the branch's own commits, newest first:")
	var loser git.CommitLineStats
	for i, c := range commits {
		if i == emptyMergeReportLimit {
			fmt.Fprintf(&b, "\n  ... and %d more", len(commits)-emptyMergeReportLimit)
			break
		}
		fmt.Fprintf(&b, "\n  %s %s (+%d -%d)", shortSHA(c.Commit), c.Subject, c.Added, c.Removed)
		if c.Added == 0 && c.Removed > loser.Removed {
			loser = c
		}
	}
	// A commit that only removes lines is the shape the incident had, where one
	// commit deleted the branch's whole payload. Naming the largest one is a
	// lead to check, not a verdict: a branch that legitimately only deletes
	// files reaches here too, and reads the same way.
	if loser.Commit != "" {
		fmt.Fprintf(&b, "\n%s removes %d lines and adds none — check it first",
			shortSHA(loser.Commit), loser.Removed)
	}
	return b.String()
}
