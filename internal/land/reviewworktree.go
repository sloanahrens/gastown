package land

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// reviewDirPrefix names the directory a review made ahead of a landing gets under WorkRoot.
// It is apart from the landing's WorkRoot/wt, so a review never touches the worktree a landing checks out.
const reviewDirPrefix = "review-"

// addReviewWorktree adds a worktree detached at base for a review made ahead of a landing.
// It lives in its own WorkRoot/review-<id> directory, created 0700 because om's raw model replies are written under it during the review.
// The Repo it returns carries the landing's committing identity, read from the head's author the way Land reads it before cutting its own worktree (landingCommitEnv): the merge commit a review makes through it names the landing committer and the seat that did the work (gt-vuv2n).
// The handle is opened through openLanding rather than l.open, so a review's git reads retry a config read that raced another process, as a landing's reads do.
// The identity read runs before gitMu is taken; only the worktree calls here hold gitMu.
// cleanup removes the worktree and the whole review directory; the caller defers it, and a second call does nothing.
func (l *Lander) addReviewWorktree(g Repo, w Work, base string) (wt Repo, dir string, cleanup func(), err error) {
	// The identity is read before the worktree is cut, so an unreadable head author is returned with no directory made, as Land refuses it.
	env, err := l.landingCommitEnv(g, w)
	if err != nil {
		return nil, "", nil, err
	}
	// Making the root and the review directory fail the same way: one return carries either error, so a refusal is one branch.
	var review string
	err = os.MkdirAll(l.WorkRoot, 0o700)
	if err == nil {
		review, err = os.MkdirTemp(l.WorkRoot, reviewDirPrefix+"*")
	}
	if err != nil {
		return nil, "", nil, err
	}
	dir = filepath.Join(review, landWorktreeName)
	l.gitMu.Lock()
	err = g.WorktreeAddDetached(dir, base)
	l.gitMu.Unlock()
	if err != nil {
		return nil, "", nil, errors.Join(err, os.RemoveAll(review))
	}
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			l.gitMu.Lock()
			defer l.gitMu.Unlock()
			// cleanup returns nothing, so the errors it cannot hand back are logged: WorktreeRemove of the worktree, RemoveAll of the review directory that holds it, and WorktreePrune.
			// The review directory goes whatever git said, since the raw replies under it must not outlive the review.
			// What a removal that fails leaves behind is the boot sweep's to clear (SweepReviewDirs, gt-sqy9ss), the backstop for the leftovers.
			// These remove steps repeat addWorktree's removeWorktree closure in internal/land/land.go: the bead that created addReviewWorktree must not edit addWorktree.
			if err := g.WorktreeRemove(dir, true); err != nil {
				l.logf("review worktree %s: remove: %v", dir, err)
			}
			if err := os.RemoveAll(review); err != nil {
				l.logf("review directory %s: remove: %v", review, err)
			}
			if err := g.WorktreePrune(); err != nil {
				l.logf("review worktree %s: prune: %v", dir, err)
			}
		})
	}
	return l.openLanding(dir, env), dir, cleanup, nil
}
