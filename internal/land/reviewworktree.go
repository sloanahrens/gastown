package land

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// reviewDirPrefix names the directory a review made ahead of a landing gets
// under WorkRoot. It is apart from the landing's WorkRoot/wt, so a review never
// touches the worktree a landing checks out.
const reviewDirPrefix = "review-"

// addReviewWorktree adds a worktree detached at base for a review made ahead of
// a landing. It lives in its own WorkRoot/review-<id> directory, created 0700
// because om's raw model replies are written under it during the review. The
// git calls here hold gitMu; Land's own git calls do not take it yet.
// cleanup removes the worktree and the whole review directory; the caller
// defers it, and a second call does nothing.
func (l *Lander) addReviewWorktree(g Repo, base string) (wt Repo, dir string, cleanup func(), err error) {
	if err := os.MkdirAll(l.WorkRoot, 0o700); err != nil {
		return nil, "", nil, err
	}
	review, err := os.MkdirTemp(l.WorkRoot, reviewDirPrefix+"*")
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
			// cleanup is deferred, so git's error has no caller to return to: it
			// is logged. The review directory goes whatever git said, since the
			// raw replies under it must not outlive the review; the prune after
			// that drops the registration of a worktree git could not remove.
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
	return l.open(dir), dir, cleanup, nil
}
