package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// doneRepo is the git surface of gt done's submit path: the reads and writes
// submitForLanding, rebaseOntoTarget, completeWithoutCode and the push
// helpers make on the polecat's worktree. *git.Git implements it; unit tests
// hand submitForLanding an in-memory repo instead.
type doneRepo interface {
	divergedPushGit
	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	CleanBaseRef(remote, defaultBranch, target string) string
	CommitsAhead(base, branch string) (int, error)
	Rebase(onto string) error
	GetConflictingFiles() ([]string, error)
	AbortRebase() error
	BranchPushedToRemote(localBranch, remote string) (bool, int, error)
	ForkBackedRemote(remote string) bool
	VerifyPushedCommitReachableFromPushTarget(remote, branch, commit string) error
	VerifyPushedCommit(remote, branch, commit string) error
	PushRemoteBranchTip(remote, branch string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
}

var _ doneRepo = (*git.Git)(nil)

// doneSubmitDeps are the collaborators of the submit path. runDone wires the
// real ones (useRealSubmitDeps); a unit test builds a doneRun with fakes and
// calls submitForLanding directly, so nothing is spawned and no package
// global or process environment is read.
type doneSubmitDeps struct {
	repo doneRepo
	// source resolves the work bead through town routing and returns it
	// with the client of the database that owns it.
	source func(issueID string) (*beads.Issue, beads.Client, error)
	// localGate builds the pre-submit gate for the rig.
	localGate func(townRoot, rigName, dir string) (land.Gate, error)
	// checkBranch refuses a rebased branch that reverts merged work, adds
	// throwaway files, or repeats a rejected attempt byte for byte.
	checkBranch func(baseRef string, sub doneSubmission) error
	// rewriteBranch squashes auto-save commits and strips attribution
	// trailers and the Gas Town overlay before the gate sees the branch.
	rewriteBranch func(baseRef string, sub doneSubmission) error
	// pushSubmodules pushes changed submodule commits before the branch.
	pushSubmodules func(baseRef string)
	// sleep waits between push and close retries.
	sleep func(time.Duration)
}

// doneOptions are the flags and environment the submit path reads.
type doneOptions struct {
	target              string
	allowReverts        bool
	allowThrowawayPaths bool
	// polecatEnv is GT_POLECAT being set: a polecat must bring at least one
	// commit unless the branch is already pushed or the work is non-code.
	polecatEnv bool
	// preVerified skips the local gate. Only the crew path honors it.
	preVerified bool
}

// useRealSubmitDeps wires the submit path to the worktree's git, routed bd,
// the rig's gate and the real clock, and reads gt done's flags.
func (r *doneRun) useRealSubmitDeps(getenv func(string) string) {
	r.opts = doneOptions{
		target:              doneTarget,
		allowReverts:        doneAllowReverts,
		allowThrowawayPaths: doneAllowThrowawayPaths,
		polecatEnv:          getenv("GT_POLECAT") != "",
	}
	r.deps = doneSubmitDeps{
		repo: r.g,
		source: func(issueID string) (*beads.Issue, beads.Client, error) {
			info, err := resolveSubmitSourceIssue(r.cwd, issueID)
			if err != nil {
				return nil, nil, err
			}
			var client beads.Client
			if info.BD != nil {
				client = info.BD
			}
			return info.Issue, client, nil
		},
		localGate:      doneLocalGate,
		checkBranch:    func(baseRef string, sub doneSubmission) error { return checkBranchForSubmit(r, sub, baseRef) },
		rewriteBranch:  func(baseRef string, sub doneSubmission) error { return rewriteBranchForSubmit(r, sub, baseRef) },
		pushSubmodules: func(baseRef string) { pushSubmoduleChanges(r.g, baseRef) },
		sleep:          time.Sleep,
	}
}
