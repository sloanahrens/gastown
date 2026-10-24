package land

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// The Reason tokens of a WorkLandedVerdict. They are machine-readable: callers
// switch on them, so anything an operator reads (file names, git errors) goes
// in Detail.
const (
	// WorkLandedByRef: the head is an ancestor of refs/landed/<bead>.
	WorkLandedByRef = "landed-ref"
	// WorkLandedByTarget: the head's own patches are already on origin/<target>.
	WorkLandedByTarget = "landed-target"
	// WorkLandedByRecord: the closed bead's LANDING RECORD names content the
	// worktree matches for every file its commits touched.
	WorkLandedByRecord = "landed-record"
	// WorkLandedDiffers: the bead landed, but not as this worktree's content;
	// Detail names the files to compare.
	WorkLandedDiffers = "landed-differs"
	// WorkUnpushed: nothing proves the work landed.
	WorkUnpushed = "unpushed"
	// WorkProbeFailed: a read the proof needs failed, so nothing is proven;
	// Detail says which.
	WorkProbeFailed = "probe-failed"
)

// WorkLandedVerdict is WorkLandedOnTarget's answer. Landed is true only for a
// proof; every other verdict carries a Reason that says why not.
type WorkLandedVerdict struct {
	Landed bool
	Reason string
	Detail string
}

// WorkLandedRepo is the repository surface WorkLandedOnTarget reads.
type WorkLandedRepo interface {
	Rev(ref string) (string, error)
	RefExists(ref string) (bool, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	MergeBase(a, b string) (string, error)
	CommitLandedOnTarget(remote, target, commit string) bool
	DiffNameOnly(base, head string) ([]string, error)
	DiffPathsNameOnly(a, b string, paths []string) ([]string, error)
	RemoteDefaultBranch() string
}

// WorkLandedQuery is what WorkLandedOnTarget is asked about. The fields are
// named because four of the five are strings, and a caller that swapped two of
// them would still compile (gt-cmutl).
type WorkLandedQuery struct {
	// Head is the worktree HEAD ("" reads the repository's HEAD).
	Head string
	// Target is the integration branch on origin ("" means the rig's
	// configured default branch, which is what a landing targets).
	Target string
	// RigPath is the rig root whose config names that default branch ("" falls
	// back to the remote's HEAD, for a caller with no rig to ask).
	RigPath string
	// BeadID names refs/landed/<bead> ("" skips the ancestry signal).
	BeadID string
	// Issue is BeadID as read (nil when unreadable).
	Issue *beads.Issue
}

// WorkLandedOnTarget reports whether the work the worktree at g holds is
// already on the integration target.
//
// A query proves it landed when any one arm does:
//
//   - Head is an ancestor of refs/landed/<BeadID> (WorkLandedByRef);
//   - Head's own patches are already on origin/<Target> (WorkLandedByTarget);
//   - the bead is closed with a LANDING RECORD and every file the worktree's
//     commits touched has the content the record names (WorkLandedByRecord).
//
// It fails closed: a missing landing record, a ref or object that will not
// read, and a worktree that touched no files all return Landed false with a
// Reason, never a landed answer. A failed read in one arm does not hide a
// proof another arm makes.
//
// Every read is local. refs/landed/<bead> and origin's tracking refs live in
// the repository a seat's worktree shares, so a seat whose branch was deleted
// after its landing is still answerable without a network round trip.
func WorkLandedOnTarget(g WorkLandedRepo, q WorkLandedQuery) WorkLandedVerdict {
	if g == nil {
		return probeFailed("no repository to read")
	}
	head := strings.TrimSpace(q.Head)
	if head == "" {
		rev, err := g.Rev("HEAD")
		if err != nil {
			return probeFailed(fmt.Sprintf("read HEAD: %v", err))
		}
		head = strings.TrimSpace(rev)
	}
	if head == "" {
		return probeFailed("read HEAD: empty revision")
	}

	var failures []string

	// refs/landed/<bead> holds the head the landing took from this branch, so
	// a worktree left at an earlier point of a branch that another seat
	// carried on and landed is its ancestor — the state no branch-presence or
	// per-commit check can recognize (gt-jol0i).
	if beadID := strings.TrimSpace(q.BeadID); beadID != "" {
		landed, err := headInLandedRef(g, head, LandedRef(beadID))
		if err != nil {
			failures = append(failures, err.Error())
		} else if landed {
			return WorkLandedVerdict{Landed: true, Reason: WorkLandedByRef}
		}
	}

	target := strings.TrimSpace(q.Target)
	if target == "" {
		rigTarget, err := RigDefaultBranch(q.RigPath)
		if err != nil {
			return probeFailed(fmt.Sprintf("resolve the rig's default branch: %v", err))
		}
		target = rigTarget
	}
	if target == "" {
		target = g.RemoteDefaultBranch()
	}
	if target == "" {
		target = "main"
	}
	if g.CommitLandedOnTarget("origin", target, head) {
		return WorkLandedVerdict{Landed: true, Reason: WorkLandedByTarget}
	}

	v := worktreeMatchesLandingRecord(g, head, target, q.Issue)
	if v.Landed || v.Reason == WorkLandedDiffers {
		return v
	}
	if v.Reason == WorkProbeFailed {
		failures = append(failures, v.Detail)
	}
	if len(failures) > 0 {
		return probeFailed(strings.Join(failures, "; "))
	}
	return v
}

// RigDefaultBranch is the branch a rig's landings target: the rig config's
// default_branch, else "main". A config that exists but does not decode is an
// error, never the "main" fallback (gt-v4r0x).
func RigDefaultBranch(rigPath string) (string, error) {
	if rigPath = strings.TrimSpace(rigPath); rigPath == "" {
		return "", nil
	}
	cfg, err := config.LoadRigConfig(filepath.Join(rigPath, "config.json"))
	if errors.Is(err, config.ErrNotFound) {
		return "main", nil
	}
	if err != nil {
		return "", err
	}
	if cfg != nil && cfg.DefaultBranch != "" {
		return cfg.DefaultBranch, nil
	}
	return "main", nil
}

func probeFailed(detail string) WorkLandedVerdict {
	return WorkLandedVerdict{Reason: WorkProbeFailed, Detail: detail}
}

// headInLandedRef reports whether head is the commit refs/landed/<bead> holds
// or an ancestor of it. A ref that does not exist is a plain "no" (a landing
// that predates the ref); a ref that exists and will not read is an error.
func headInLandedRef(g WorkLandedRepo, head, ref string) (bool, error) {
	exists, err := g.RefExists(ref)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", ref, err)
	}
	if !exists {
		return false, nil
	}
	landedHead, err := g.Rev(ref)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", ref, err)
	}
	landedHead = strings.TrimSpace(landedHead)
	if landedHead == "" {
		return false, fmt.Errorf("read %s: empty revision", ref)
	}
	if landedHead == head {
		return true, nil
	}
	ancestor, err := g.IsAncestor(head, landedHead)
	if err != nil {
		return false, fmt.Errorf("compare HEAD with %s: %w", ref, err)
	}
	return ancestor, nil
}

// worktreeMatchesLandingRecord is WorkLandedOnTarget's content arm: a closed
// bead whose LANDING RECORD names a commit, and whose tree agrees with the
// worktree's for every file the worktree's own commits touched.
//
// Restricting the comparison to those files is what lets an earlier point of a
// continued branch count: the later commits that carried the branch on are not
// this worktree's, so the files only they touched say nothing about it.
func worktreeMatchesLandingRecord(g WorkLandedRepo, head, target string, issue *beads.Issue) WorkLandedVerdict {
	unproven := func(detail string) WorkLandedVerdict {
		return WorkLandedVerdict{Reason: WorkUnpushed, Detail: detail}
	}
	if issue == nil {
		return unproven("bead not read")
	}
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		return unproven("bead " + issue.ID + " is not closed")
	}
	rec, ok := ParseLandingRecord(issue.Notes)
	if !ok {
		return unproven("closed bead " + issue.ID + " has no landing record")
	}
	// The record's head is the tree the work landed as; landed_commit is the
	// same landing rewritten onto the target, and stands in when the head is
	// no longer reachable here.
	landedRef := strings.TrimSpace(rec.Head)
	if landedRef == "" {
		landedRef = strings.TrimSpace(rec.LandedCommit)
	}
	// rev-parse echoes a full SHA whether or not the object exists, so the
	// check peels it to a commit, which reads the object (gt-3xegd).
	if _, err := g.Rev(landedRef + "^{commit}"); err != nil {
		landedRef = strings.TrimSpace(rec.LandedCommit)
		if _, err := g.Rev(landedRef + "^{commit}"); err != nil {
			return probeFailed(fmt.Sprintf("landing record of %s names no commit this repository holds", issue.ID))
		}
	}

	base, err := g.MergeBase(head, "origin/"+target)
	if err != nil {
		return probeFailed(fmt.Sprintf("merge-base of HEAD and origin/%s: %v", target, err))
	}
	touched, err := g.DiffNameOnly(base, head)
	if err != nil {
		return probeFailed(fmt.Sprintf("list files the worktree's commits touched: %v", err))
	}
	if len(touched) == 0 {
		// An empty comparison equals anything: no file is left to disagree, so
		// it proves nothing about what the worktree holds.
		return unproven("worktree's commits touch no files to compare with the landing record")
	}
	diff, err := g.DiffPathsNameOnly(head, landedRef, touched)
	if err != nil {
		return probeFailed(fmt.Sprintf("compare worktree with the landing record: %v", err))
	}
	if len(diff) == 0 {
		return WorkLandedVerdict{Landed: true, Reason: WorkLandedByRecord}
	}
	return WorkLandedVerdict{
		Reason: WorkLandedDiffers,
		Detail: "landed from another head; worktree differs in " + strings.Join(diff, ", "),
	}
}
