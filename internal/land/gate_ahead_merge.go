package land

import "fmt"

// gateAheadMergeSlot is the landing slot a second candidate is built in: slot
// 0 holds the landing's own worktree (gt-xwlcrw).
const gateAheadMergeSlot = 1

// gateAheadMerge cuts the landing worktree in slot 1 at base and merges w into
// it with the landing's commit identity, returning the merged commit and the
// cleanup that removes the worktree. It writes nothing to the bead: this is a
// train's second candidate, not a landing, so a conflict or failed merge comes
// back as the reason and is never recorded (train 3b-1). A failure removes the
// worktree it made and returns a nil cleanup; what a panic leaves, the next
// add's stale-slot clear removes (addWorktree).
func (l *Lander) gateAheadMerge(g Repo, w Work, base string) (dir, merged string, cleanup func(), err error) {
	// The identity is read before the worktree is cut, as mergeCar reads it: an unreadable head author fails with no directory made (gt-vuv2n).
	env, err := l.landingCommitEnv(g, w)
	if err != nil {
		return "", "", nil, &InfraError{Stage: "head author", Err: fmt.Errorf("head %s: %w", shortSHA(w.Head), err)}
	}
	var cleanupFn func()
	if err := l.withGitMu(func() error {
		var addErr error
		dir, _, cleanupFn, addErr = l.addWorktree(g, base, gateAheadMergeSlot)
		return addErr
	}); err != nil {
		return "", "", nil, &InfraError{Stage: "worktree", Err: err}
	}
	out, err := mergeWork(l.openLanding(dir, env), w, base)
	if err != nil {
		cleanupFn()
		return "", "", nil, err
	}
	if out.Rejection != nil {
		cleanupFn()
		return "", "", nil, out.Rejection
	}
	return dir, out.Commit, cleanupFn, nil
}
