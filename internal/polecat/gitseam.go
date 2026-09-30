package polecat

import (
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// gitRepo is the part of *git.Git polecat works through: the rig's repo base
// (the shared .repo.git or mayor/rig), each polecat worktree, and the
// holders of branches it claims (docs/testing.md, "Seams for external
// tools"). Unit tests open gitfake repositories instead.
type gitRepo interface {
	git.IndexSkewClassifier

	TopLevel() (string, error)
	GitDir() (string, error)
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error

	Rev(ref string) (string, error)
	RefExists(ref string) (bool, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	FirstParentContains(commit, descendant string) (bool, error)
	Cherry(upstream, head string) (string, error)
	CountCommitsBehind(ref string) (int, error)
	CurrentBranch() (string, error)
	ListBranches(pattern string) ([]string, error)
	DeleteBranch(name string, force bool) error
	RemoteDefaultBranch() string

	Fetch(remote string) error
	FetchBranch(remote, branch string) error
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	Push(remote, branch string, force bool) error
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	ListRemoteRefsWithHashesTimeout(remote, prefix string, timeout time.Duration) ([]git.RemoteRef, error)

	Checkout(ref string) error
	CheckoutNewBranch(branch, startPoint string) error
	CheckoutResetBranch(branch, startPoint string) error
	CheckoutDetachForce(ref string) error
	ResetHard(ref string) error
	CleanForce() error

	WorktreeAddFromRef(path, branch, startPoint string) error
	WorktreeAddExistingForce(path, branch string) error
	WorktreeRemove(path string, force bool) error
	WorktreeMove(oldPath, newPath string) error
	WorktreePrune() error
	WorktreeList() ([]git.Worktree, error)

	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	CheckUncommittedWorkLocal() (*git.UncommittedWorkStatus, error)
	BranchPushedToRemote(localBranch, remote string) (bool, int, error)
	BranchPreservationStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error)
	BranchTargetStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error)
	RefPreservedByRef(head, ref string) (git.BranchPreservationStatus, error)
}

var _ gitRepo = (*git.Git)(nil)

// gitOpener opens git on a working directory (open) or on a bare
// repository's git directory (openDir). Its zero value opens *git.Git; tests
// set both funcs to a gitfake world's openers.
type gitOpener struct {
	open    func(workDir string) gitRepo
	openDir func(gitDir, workDir string) gitRepo
}

// Open is git.NewGit(workDir), or the test's opener.
func (o gitOpener) Open(workDir string) gitRepo {
	if o.open == nil {
		return git.NewGit(workDir)
	}
	return o.open(workDir)
}

// OpenDir is git.NewGitWithDir(gitDir, workDir), or the test's opener.
func (o gitOpener) OpenDir(gitDir, workDir string) gitRepo {
	if o.openDir == nil {
		return git.NewGitWithDir(gitDir, workDir)
	}
	return o.openDir(gitDir, workDir)
}
