package refinery

import (
	"fmt"
	"path/filepath"
)

// prepareMergeTarget stages the working tree on the current tip of origin/target,
// ready for the merge commits that will be pushed back to origin/target.
//
// A plain `git checkout target; git pull` is wrong here for two reasons that
// both follow from sharing .repo.git with the polecat worktrees (gt-032w): a
// live worktree holding target makes the checkout fail for as long as it stays
// on the branch, which blocks every merge in the rig; and a local target ref
// left ahead of origin keeps the pull from moving it, so the merge would
// publish whatever another agent committed there.
func (e *Engineer) prepareMergeTarget(target string) error {
	detached, holder, err := e.stageOnMergeTarget(target)
	if err != nil {
		return err
	}
	if detached {
		_, _ = fmt.Fprintf(e.output, "[Engineer] %s is checked out at %s — staging on a detached HEAD at origin/%s\n", target, holder, target)
	}

	// Best-effort: a merge built on a slightly stale origin/target still has
	// the right shape, and the push refuses if target moved under it.
	if pullErr := e.git.Pull("origin", target); pullErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: pull from origin/%s: %v (continuing)\n", target, pullErr)
	}

	// The pull fast-forwards a target that is behind origin but leaves one that
	// is ahead of it where it is, so snap to origin explicitly (gt-032w).
	if resetErr := e.git.ResetHard("origin/" + target); resetErr != nil {
		return fmt.Errorf("reset %s to origin/%s: %w", target, target, resetErr)
	}
	return nil
}

// stageOnMergeTarget leaves the working tree on target, or on a detached HEAD
// at origin/target when another worktree holds target. It reports the holding
// worktree's path in that case, because the local target ref then stays where
// it is.
func (e *Engineer) stageOnMergeTarget(target string) (detached bool, holder string, err error) {
	checkoutErr := e.git.Checkout(target)
	if checkoutErr == nil {
		return false, "", nil
	}

	holdingPath, held := e.worktreeHolding(target)
	if !held {
		return false, "", fmt.Errorf("checkout target %s: %w", target, checkoutErr)
	}
	if detachErr := e.git.CheckoutDetach("origin/" + target); detachErr != nil {
		return false, "", fmt.Errorf("checkout target %s (held by %s): %w", target, holdingPath, detachErr)
	}
	return true, holdingPath, nil
}

// worktreeHolding reports the path of a worktree other than this one with
// target checked out.
func (e *Engineer) worktreeHolding(target string) (string, bool) {
	worktrees, err := e.git.WorktreeList()
	if err != nil {
		return "", false
	}
	self := e.git.WorkDir()
	for _, wt := range worktrees {
		if wt.Branch == target && !sameDir(wt.Path, self) {
			return wt.Path, true
		}
	}
	return "", false
}

// restoreTargetToOrigin makes local <target> exactly match origin/<target>,
// regardless of how this worktree is staged: attached to target (ResetHard
// moves the checked-out branch) or detached because another worktree holds
// target. `git branch -f` cannot move a ref another worktree's HEAD points
// at, but a run staged detached for that reason never advanced refs/heads/
// target in the first place, so there is nothing to restore there. Every
// batch-processing exit path must call this before returning without having
// pushed — a run that left target mid-bisection, ahead of origin and holding
// a rejected stacked merge, let the next push carry that merge to origin
// (gt-u093).
func (e *Engineer) restoreTargetToOrigin(target string) error {
	current, err := e.git.CurrentBranch()
	if err != nil {
		return fmt.Errorf("resolve current branch before restoring %s: %w", target, err)
	}
	if current == target {
		return e.git.ResetHard("origin/" + target)
	}
	if _, held := e.worktreeHolding(target); held {
		return nil
	}
	if exists, existsErr := e.git.BranchExists(target); existsErr != nil || !exists {
		return nil
	}
	return e.git.ResetBranch(target, "origin/"+target)
}

// refuseIfTargetAhead refuses to start a merge cycle when local <target>
// already holds commits origin/<target> doesn't have. prepareMergeTarget's
// own reset-to-origin silently discards exactly that state on every call it
// makes during stack construction and bisection, which is correct there —
// but if the discarded commits are a rejected stacked merge left behind by a
// run that exited without calling restoreTargetToOrigin (gt-u093), silent
// discard is also how that merge would slip into the next successful push
// unnoticed. So this checks once, before the first prepareMergeTarget call of
// a cycle, and refuses loudly instead of quietly resetting through it.
func (e *Engineer) refuseIfTargetAhead(target string) error {
	if err := e.git.Fetch("origin"); err != nil {
		return fmt.Errorf("fetch origin before ahead-of-origin check on %s: %w", target, err)
	}
	if exists, err := e.git.BranchExists(target); err != nil || !exists {
		return nil
	}
	localSHA, err := e.git.Rev(target)
	if err != nil {
		return nil
	}
	originSHA, err := e.git.Rev("origin/" + target)
	if err != nil {
		return fmt.Errorf("resolve origin/%s: %w", target, err)
	}
	if localSHA == originSHA {
		return nil
	}
	ahead, err := e.git.IsAncestor("origin/"+target, target)
	if err != nil {
		return fmt.Errorf("check whether %s is ahead of origin/%s: %w", target, target, err)
	}
	if !ahead {
		// Diverged or behind origin — a different problem than the one this
		// guards against; prepareMergeTarget's reset-to-origin handles it safely.
		return nil
	}
	return fmt.Errorf("local %s (%s) is ahead of origin/%s (%s) — refusing to build on it; a prior run may have exited without restoring %s (see gt-u093)", target, shortSHA(localSHA), target, shortSHA(originSHA), target)
}

// mergePushRef is the refspec that lands a merge staged on target. HEAD holds
// the merge tip whether the staging tree is attached to target or detached at
// origin/target, and naming it keeps the landing push independent of a local
// target ref the merge no longer owns (gt-032w).
func mergePushRef(target string) string {
	return "HEAD:refs/heads/" + target
}

// sameDir compares two directory paths after resolving symlinks, which differ
// between git's reported worktree paths and the paths gt hands to git on macOS
// (/tmp against /private/tmp).
func sameDir(a, b string) bool {
	return resolveDir(a) == resolveDir(b)
}

func resolveDir(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
