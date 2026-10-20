package land

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// WorkLandedRepo is the repository surface WorkLandedOnTarget reads.
type WorkLandedRepo interface {
	Rev(ref string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	MergeBase(a, b string) (string, error)
	CommitLandedOnTarget(remote, target, commit string) bool
	DiffNameOnly(base, head string) ([]string, error)
	DiffPathsNameOnly(a, b string, paths []string) ([]string, error)
	RemoteDefaultBranch() string
}

// WorkLandedOnTarget reports whether the work the worktree at g holds is
// already on the integration target, and the reason when it is not.
//
// head is the worktree HEAD ("" reads the repository's HEAD), target is the
// integration branch on origin ("" means the remote's default), beadID names
// refs/landed/<bead> ("" skips the ancestry signal), and issue is that bead as
// read (nil when unreadable). The answers are:
//
//   - (true, "landed"): head is an ancestor of refs/landed/<bead>, its own
//     patches are already on origin/<target>, or every file its commits
//     touched has the content the closed bead's LANDING RECORD names;
//   - (false, "landed from another head; worktree differs in <files>"): the
//     bead landed, but those files disagree with what landed, so they are what
//     an operator must compare before discarding the worktree;
//   - (false, ""): nothing proves this work is landed.
//
// Every read is local. refs/landed/<bead> and origin's tracking refs live in
// the repository a seat's worktree shares, so a seat whose branch was deleted
// after its landing is still answerable without a network round trip.
func WorkLandedOnTarget(g WorkLandedRepo, head, target, beadID string, issue *beads.Issue) (bool, string) {
	if g == nil {
		return false, ""
	}
	head = strings.TrimSpace(head)
	if head == "" {
		rev, err := g.Rev("HEAD")
		if err != nil {
			return false, ""
		}
		head = strings.TrimSpace(rev)
	}
	if head == "" {
		return false, ""
	}

	// refs/landed/<bead> holds the head the landing took from this branch, so
	// a worktree left at an earlier point of a branch that another seat
	// carried on and landed is its ancestor — the state no branch-presence or
	// per-commit check can recognize (gt-jol0i).
	if beadID = strings.TrimSpace(beadID); beadID != "" {
		if landedHead, err := g.Rev(LandedRef(beadID)); err == nil && strings.TrimSpace(landedHead) != "" {
			landedHead = strings.TrimSpace(landedHead)
			if landedHead == head {
				return true, "landed"
			}
			if ancestor, err := g.IsAncestor(head, landedHead); err == nil && ancestor {
				return true, "landed"
			}
		}
	}

	if target = strings.TrimSpace(target); target == "" {
		target = g.RemoteDefaultBranch()
	}
	if target == "" {
		target = "main"
	}
	if g.CommitLandedOnTarget("origin", target, head) {
		return true, "landed"
	}
	return worktreeMatchesLandingRecord(g, head, target, issue)
}

// worktreeMatchesLandingRecord is WorkLandedOnTarget's content arm: a closed
// bead whose LANDING RECORD names a commit, and whose tree agrees with the
// worktree's for every file the worktree's own commits touched.
//
// Restricting the comparison to those files is what lets an earlier point of a
// continued branch count: the later commits that carried the branch on are not
// this worktree's, so the files only they touched say nothing about it.
func worktreeMatchesLandingRecord(g WorkLandedRepo, head, target string, issue *beads.Issue) (bool, string) {
	if issue == nil || !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		return false, ""
	}
	rec, ok := ParseLandingRecord(issue.Notes)
	if !ok {
		return false, ""
	}
	// The record's head is the tree the work landed as; landed_commit is the
	// same landing rewritten onto the target, and stands in when the head is
	// no longer reachable here.
	landedRef := strings.TrimSpace(rec.Head)
	if landedRef == "" {
		landedRef = strings.TrimSpace(rec.LandedCommit)
	}
	if _, err := g.Rev(landedRef); err != nil {
		landedRef = strings.TrimSpace(rec.LandedCommit)
		if _, err := g.Rev(landedRef); err != nil {
			return false, ""
		}
	}

	base, err := g.MergeBase(head, "origin/"+target)
	if err != nil {
		return false, ""
	}
	touched, err := g.DiffNameOnly(base, head)
	if err != nil {
		return false, ""
	}
	if len(touched) == 0 {
		return true, "landed"
	}
	diff, err := g.DiffPathsNameOnly(head, landedRef, touched)
	if err != nil {
		return false, ""
	}
	if len(diff) == 0 {
		return true, "landed"
	}
	return false, "landed from another head; worktree differs in " + strings.Join(diff, ", ")
}
