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
