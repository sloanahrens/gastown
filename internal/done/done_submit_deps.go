package done

import (
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/nudge"
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
	// retryDelays is the wait schedule before each re-attempt of the branch
	// push (see landBranchPush).
	retryDelays []time.Duration
	// polecatSeat maps a bead assignee to the tmux session of the polecat seat
	// it names; ok is false for any assignee that is not a polecat
	// (<rig>/polecats/<name>). The crew submit path uses it to stand a holder
	// down (gt-qmnm3).
	polecatSeat func(assignee string) (sessionName string, ok bool)
	// sessionAlive reports whether a tmux session name is live.
	sessionAlive func(sessionName string) bool
	// enqueueNudge queues a nudge to a session; nil on the polecat path, which
	// never stands another seat down.
	enqueueNudge func(townRoot, sessionName string, n nudge.QueuedNudge) error
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

// doneRetrySleep is the wait between the real submit path's push and close
// retries. It is a variable so an end-to-end test of a failing close does not
// spend the ladder's six seconds.
var doneRetrySleep = time.Sleep

// submitSeams resolves the process seams the submit path reads: the local
// gate, the retry sleep and the push retry ladder. Options may override each
// so the command layer's integration tests can substitute them; nil means the
// real one (see Options).
func submitSeams(o Options) (localGate func(townRoot, rigName, dir string) (land.Gate, error), sleep func(time.Duration), delays []time.Duration) {
	localGate = doneLocalGate
	if o.LocalGate != nil {
		localGate = o.LocalGate
	}
	sleep = doneRetrySleep
	if o.Sleep != nil {
		sleep = o.Sleep
	}
	delays = pushLandingRetryDelays
	if o.RetryDelays != nil {
		delays = o.RetryDelays
	}
	return localGate, sleep, delays
}

// useRealSubmitDeps wires the submit path to the worktree's git, routed bd,
// the rig's gate and the real clock, and reads gt done's options.
func (r *doneRun) useRealSubmitDeps(o Options) {
	r.opts = doneOptions{
		target:              o.Target,
		allowReverts:        o.AllowReverts,
		allowThrowawayPaths: o.AllowThrowawayPaths,
		polecatEnv:          r.env("GT_POLECAT") != "",
	}

	localGate, sleep, delays := submitSeams(o)

	r.deps = doneSubmitDeps{
		repo: r.g,
		source: func(issueID string) (*beads.Issue, beads.Client, error) {
			info, err := resolveSubmitSourceIssue(r.cwd, issueID)
			if err != nil {
				return nil, nil, err
			}
			return info.Issue, info.BD, nil
		},
		localGate:      localGate,
		checkBranch:    func(baseRef string, sub doneSubmission) error { return checkBranchForSubmit(r, sub, baseRef) },
		rewriteBranch:  func(baseRef string, sub doneSubmission) error { return rewriteBranchForSubmit(r, sub, baseRef) },
		pushSubmodules: func(baseRef string) { pushSubmoduleChanges(r.g, baseRef) },
		sleep:          sleep,
		retryDelays:    delays,
	}
}
