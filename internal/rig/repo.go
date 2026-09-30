package rig

import (
	"github.com/steveyegge/gastown/internal/git"
)

// Repo is the part of *git.Git a rig works through (docs/testing.md, "Seams
// for external tools"): the town's git that clones and probes remotes, and
// each repository or worktree a rig holds. *git.Git satisfies it in
// production; tests pass gitfake.
type Repo interface {
	CloneBareWithBranch(url, dest, branch string) error
	CloneBareWithReferenceAndBranch(url, dest, reference, branch string) error
	CloneBarePartialWithBranch(url, dest, filter, branch string) error
	CloneBarePartialWithReferenceAndBranch(url, dest, filter, reference, branch string) error
	CloneBranch(url, dest, branch string) error
	CloneBranchWithReference(url, dest, branch, reference string) error
	CloneBranchPartial(url, dest, branch, filter string) error
	CloneBranchPartialWithReference(url, dest, branch, filter, reference string) error
	RemoteHasRefs(remote string) (bool, error)
	FetchBranchShallow(remote, branch string) error
	RemoteURL(remote string) (string, error)
	GetPushURL(remote string) (string, error)
	ConfigurePushURL(remote, pushURL string) error
	ClearPushURL(remote string) error
	AddUpstreamRemote(upstreamURL string) error
	IsRepo() bool
	IsEmpty() (bool, error)
	DefaultBranch() string
	RefExists(ref string) (bool, error)
	CommonDir() (string, error)
}

var _ Repo = (*git.Git)(nil)

// openGitDir opens git.NewGitWithDir(gitDir, workDir); git.NewGit(dir) is
// openGitDir("", dir).
func (m *Manager) openGitDir(gitDir, workDir string) Repo {
	if m.openRepo != nil {
		return m.openRepo(gitDir, workDir)
	}
	return git.NewGitWithDir(gitDir, workDir)
}

func (m *Manager) openGit(dir string) Repo { return m.openGitDir("", dir) }
