package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// daemonGit is the git surface the daemon drives, declared from the *git.Git
// methods it calls. Production opens a *git.Git per directory; unit tests
// open internal/git/gitfake's model through openGitFn, so no git runs.
type daemonGit interface {
	Rev(ref string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	CommitTime(rev string) (time.Time, error)
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	WorktreeAddDetached(path, ref string) error
	WorktreeRemove(path string, force bool) error

	// Staging, for the checkpoint dog.
	Status() (*git.GitStatus, error)
	Add(pathspecs ...string) error
	ResetFiles(pathspecs ...string) error
	StagedChanges() ([]git.StagedChange, error)
	WriteTree() (string, error)
	Commit(message string) error
	CleanDefaultBranchBaseRef(remote, defaultBranch string) string
	git.RevertReader
}

var _ daemonGit = (*git.Git)(nil)

// gitAt opens the repository or worktree at dir: openGitFn's when a test set
// one, else a *git.Git.
func (d *Daemon) gitAt(dir string) daemonGit {
	if d.openGitFn != nil {
		return d.openGitFn(dir)
	}
	return git.NewGit(dir)
}
