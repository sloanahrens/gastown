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

	// GitDir locates the directory that holds a worktree's MERGE_HEAD and
	// rebase-* markers, so the checkpoint dog can tell a mid-conflict
	// worktree from a dirty one (gt-kbp1t).
	GitDir() (string, error)
	git.RevertReader

	// The JSONL backup repository.
	InitRepo(branch string) error
	ConfigSet(key, value string) error
	RemoteURL(remote string) (string, error)
	CurrentBranch() (string, error)
	CommitWithAuthor(message, author string) error
	Push(remote, refspec string, force bool) error
	PushWithTimeout(remote, refspec string, force bool, timeout time.Duration) error
	PackSize() (string, error)
	LogAll(max int) ([]git.LogEntry, error)

	// Rig repository upkeep, for the git_hygiene patrol.
	FetchPrune(remote string) error
	RemoteDefaultBranch() string
	ListBranches(pattern string) ([]string, error)
	DeleteBranch(name string, force bool) error
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
	GC() error
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
