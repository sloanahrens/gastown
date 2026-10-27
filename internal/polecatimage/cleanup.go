package polecatimage

import "os"

// Cleanup removes what this run left behind: the publish scratch directory, the
// build's worktree, and the lock while this run's pid is still in it.
//
// After a takeover the lock directory is another run's, and removing it would
// let a third run build beside that one, so a run that lost the lock leaves it.
// Every step is best effort: Cleanup runs on the way out of a failed run too,
// and must not replace that run's failure with its own.
func (r *Run) Cleanup() {
	if r.TarDir != "" {
		_ = os.RemoveAll(r.TarDir)
		r.TarDir = ""
	}
	if r.Worktree != "" {
		if isDir(r.Worktree) {
			if r.call(r.Cfg.Git, "-C", r.Cfg.RigRepo, "worktree", "remove", "--force", r.Worktree).failed() {
				_ = os.RemoveAll(r.Worktree)
			}
		}
		r.Worktree = ""
	}
	if r.HaveLock && r.LockHeld() {
		_ = os.RemoveAll(r.Cfg.LockDir)
		r.HaveLock = false
	}
}
