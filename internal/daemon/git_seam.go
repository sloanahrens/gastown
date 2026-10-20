package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
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
	// CheckUncommittedWork reads the worktree's state for the overrun stop's
	// release, which records how much of it is at risk (git.CleanExcludingRuntime
	// and CleanupStatus).
	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	Add(pathspecs ...string) error
	ResetFiles(pathspecs ...string) error
	StagedChanges() ([]git.StagedChange, error)
	RuntimeArtifactPathspecs(paths []string) []string
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
	// CheckoutDetach detaches HEAD at a ref, for the overrun stop's reset of a
	// released seat's worktree.
	CheckoutDetach(ref string) error
	CommitWithAuthor(message, author string) error
	// ResetSoft moves the repository's branch to a ref and leaves its index
	// and working tree alone, so the commits it had above the ref become
	// staged content (the oversize-push recovery, gt-q30zx).
	ResetSoft(ref string) error
	// TreeFileSizes maps a rev's tree to each file's size in bytes, so the
	// backup can weigh what a push would send against the remote's per-file
	// limit without reading the working tree.
	TreeFileSizes(rev string) (map[string]int64, error)
	Push(remote, refspec string, force bool) error
	PushWithTimeout(remote, refspec string, force bool, timeout time.Duration) error
	PackSize() (string, error)
	LogAll(max int) ([]git.LogEntry, error)

	// Remotes lists the repository's remotes (with RemoteURL above, enough to
	// resolve the remote a rig lands through from its configured Forgejo URL,
	// gt-fn9e6.9).
	Remotes() ([]string, error)

	// RefExists and ShowFileAtRev read a rig repository's tracked files, which
	// is how a rig's repo-committed .gastown/settings.json tier is read
	// (gt-yj6g1).
	RefExists(ref string) (bool, error)
	ShowFileAtRev(ref, path string) (string, error)

	// Rig repository upkeep, for the git_hygiene patrol.
	FetchPrune(remote string) error
	RemoteDefaultBranch() string
	// RefreshRemoteDefaultBranch fetches only remote's default branch into
	// this clone's remote-tracking ref for it; the landing worker asks a
	// landed bead's author seat for it (gt-fn9e6.55).
	RefreshRemoteDefaultBranch(remote string) error
	ListBranches(pattern string) ([]string, error)
	DeleteBranch(name string, force bool) error
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	// Cherry reads whether a branch's commits are on a base by patch identity,
	// for the git_hygiene patrol's unpushed-work check.
	Cherry(upstream, head string) (string, error)
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

// repoFileOpener is rig.ReadRepoFile's opener over the daemon's git seam
// (docs/testing.md, "Seams for external tools"): the rigs whose
// .gastown/settings.json a patrol resolves settings from are the same
// repositories every other daemon read sees, and its git-free unit tier starts
// no git for them (gt-yj6g1).
func (d *Daemon) repoFileOpener() rig.RepoFileOpener {
	return func(repoPath string) rig.RepoFileReader { return d.gitAt(repoPath) }
}
