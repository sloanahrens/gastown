package doctor

import (
	"github.com/steveyegge/gastown/internal/git"
)

// Repo is the part of *git.Git doctor's checks work through (docs/testing.md,
// "Seams for external tools"): the town repository and the rigs' clones,
// worktrees and bare repositories they inspect and repair. *git.Git
// satisfies it in production; tests pass gitfake through
// CheckContext.openGit.
type Repo interface {
	IsRepo() bool
	GitDir() (string, error)
	CommonDir() (string, error)
	IsBareRepository() (bool, error)
	CurrentBranch() (string, error)
	DefaultBranch() string
	Rev(ref string) (string, error)
	RefExists(ref string) (bool, error)
	MergeBase(a, b string) (string, error)
	CountCommitsBehind(ref string) (int, error)
	Status() (*git.GitStatus, error)

	Remotes() ([]string, error)
	RemoteURL(remote string) (string, error)
	RemoveRemote(name string) error
	GetPushURL(remote string) (string, error)
	ConfigurePushURL(remote, pushURL string) error
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error

	Checkout(ref string) error
	CheckoutDetach(ref string) error
	PullRebase() error

	IsTracked(path string) (bool, error)
	IsIgnored(path string) (bool, error)
	PathChanged(path string) (bool, error)
	UntrackedPaths(pathspec string) ([]string, error)
	DisableSparseCheckout() error

	CloneBareWithBranch(url, dest, branch string) error
	WorktreeAddExistingForce(path, branch string) error
	WorktreeRemove(path string, force bool) error
	WorktreePrune() error
}

var _ Repo = (*git.Git)(nil)

// repoOpener opens git for a repository: its git directory (a bare
// repository) when set, else its working directory.
type repoOpener func(gitDir, workDir string) Repo

// git opens git in dir, as git.NewGit(dir) does.
func (ctx *CheckContext) git(dir string) Repo { return ctx.gitWithDir("", dir) }

// gitWithDir opens git as git.NewGitWithDir(gitDir, workDir) does.
func (ctx *CheckContext) gitWithDir(gitDir, workDir string) Repo {
	if ctx != nil && ctx.openGit != nil {
		return ctx.openGit(gitDir, workDir)
	}
	return git.NewGitWithDir(gitDir, workDir)
}
