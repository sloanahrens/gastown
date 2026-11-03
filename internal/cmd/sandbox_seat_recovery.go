package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
)

// sandboxSeatEnv is what check-recovery asks about a sandbox seat: the git state
// of its (possibly gone) sandbox and whether its branch is on the remote.
type sandboxSeatEnv struct {
	probe sandboxSeatProbe
	// hostGitState judges a gone sandbox seat from its host worktree and the
	// rig's shared repository; nil means the measurement is run for real.
	// Tests that model no repository supply a whole answer here.
	hostGitState func(gone polecat.LiveGitState, worktreePath, rigRoot, pushedBranch string) polecat.LiveGitState
	// gits opens the repositories the real measurement reads; its zero value is
	// the git binary, and a test supplies a repository model so
	// polecat.SandboxSeatHostGitStateWith runs the production measurement.
	gits polecat.SandboxSeatGitOpener
	// remoteBranch reports whether branch exists on origin, asked of the rig's
	// shared bare repo; nil means ls-remote there.
	remoteBranch func(rigPath, branch string) (bool, error)
	// survivingBranches reports every generated polecat branch on origin that
	// encodes issueID, most recently generated first; nil means the shared
	// reader over the rig's repo. It answers a sandbox seat that recorded no
	// branch, whose issue landed, whether a branch for the seat and issue
	// survives (gt-xhnn3).
	survivingBranches func(rigPath, issueID string) ([]string, error)
}

func (e sandboxSeatEnv) branchOnOrigin(rigPath, branch string) (bool, error) {
	if e.remoteBranch != nil {
		return e.remoteBranch(rigPath, branch)
	}
	return git.NewGitWithDir(filepath.Join(rigPath, ".repo.git"), "").RemoteBranchExists("origin", branch)
}

func (e sandboxSeatEnv) branchesForIssue(rigPath, issueID string) ([]string, error) {
	if e.survivingBranches != nil {
		return e.survivingBranches(rigPath, issueID)
	}
	return polecat.FindSurvivingBranchesForIssue(rigPath, issueID)
}

// judgeGoneSeat measures a gone sandbox seat from what survives on the host:
// its worktree, or the branch refs the shared repository still holds once the
// worktree directory is gone (gt-zgoo3). A test's modeled answer runs instead
// of the measurement, or its git opener runs the measurement itself.
func (e sandboxSeatEnv) judgeGoneSeat(gone polecat.LiveGitState, worktreePath, rigRoot, pushedBranch string) polecat.LiveGitState {
	if e.hostGitState != nil {
		return e.hostGitState(gone, worktreePath, rigRoot, pushedBranch)
	}
	return polecat.SandboxSeatHostGitStateWith(e.gits, gone, worktreePath, rigRoot, pushedBranch)
}

// applySandboxLostWorkFacts gives the verdict what it needs to tell lost work
// from a finished seat when the sandbox is gone: the bead the seat was working
// (its hook, else its source issue) and whether anything survives to continue
// from, a resume_branch on that bead or the branch itself on origin, read from
// the remote and never from the stale host worktree (gt-qq6rg.3). A remote that
// cannot be asked leaves the git state unknown rather than claiming either.
// seatName is the polecat's name, under which a seat that recorded no branch is
// matched (gt-xhnn3).
func applySandboxLostWorkFacts(facts *polecat.WorkstateFacts, env sandboxSeatEnv, bd issueShower, rigPath string, fields *beads.AgentFields, hookBead, sourceHint, seatName string) {
	workBead := hookBead
	if workBead == "" {
		workBead = sourceHint
	}
	if workBead == "" {
		return
	}
	unknown := func(reason string) {
		facts.GitCheckFailed = true
		facts.GitCheckFailedReason = "git_state=unknown: " + reason
		facts.GitStateSource = polecat.GitStateSourceUnknown
	}
	preserved := false
	issue, showErr := bd.Show(workBead)
	switch {
	case errors.Is(showErr, beads.ErrNotFound):
		return // the bead is gone: no unfinished work to lose
	case showErr != nil:
		unknown(fmt.Sprintf("could not read %s for a resume_branch: %v", workBead, showErr))
		return
	}
	if issue != nil && patrolscan.ResumeBranchFromNotes(issue.Notes) != "" {
		preserved = true
	} else {
		// The shared rule trims the recorded branch, so a whitespace-only
		// record asks origin nothing and fails closed as unknown, the same
		// answer the manager reaches for the same bead (gt-cu66c).
		branch, unknownReason := polecat.SandboxSeatOriginBranch(fields)
		switch {
		case unknownReason != "":
			// A seat that recorded no branch is judged by its issue instead,
			// with the manager's rule and its answer (gt-xhnn3).
			if reason := polecat.SandboxSeatLandedWithoutBranch(seatName, issue, workBead, func(issueID string) ([]string, error) {
				return env.branchesForIssue(rigPath, issueID)
			}); reason != "" {
				unknown(reason)
				return
			}
			preserved = true
		default:
			onOrigin, err := env.branchOnOrigin(rigPath, branch)
			if err != nil {
				unknown(fmt.Sprintf("could not ask origin whether %s was pushed: %v", branch, err))
				return
			}
			preserved = onOrigin
		}
	}
	facts.SandboxGone = true
	facts.SandboxWorkBead = workBead
	// Preserved means nothing here is at risk: the branch is on origin, a bead
	// names a resume_branch, or — with no branch recorded at all — the issue
	// landed and no branch for it survives (gt-xhnn3).
	facts.SandboxBranchPreserved = preserved
}

// sandboxSeatGitState turns a sandbox seat's git state into the GitState the
// recovery and nuke checks read. Once the sandbox is gone the state may carry
// the host worktree's dirt, stashes and unique commits, which keep the seat
// blocked; a gone sandbox with nothing left on the host reads clean. A running
// sandbox, or a gateway that could not be asked, is an error, which every
// caller already treats as "cannot check" and fails closed on (gt-qq6rg.2).
func sandboxSeatGitState(live polecat.LiveGitState) (*GitState, error) {
	if live.Source != polecat.GitStateSourceLive {
		reason := live.FailedReason
		if reason == "" {
			reason = "git state of the sandbox seat is unknown"
		}
		return nil, errors.New(reason)
	}
	return &GitState{
		Clean:            !live.Dirty && live.StashCount == 0 && live.UnpushedCommits == 0,
		UncommittedFiles: []string{},
		UnpushedCommits:  live.UnpushedCommits,
		StashCount:       live.StashCount,
	}, nil
}

// sandboxSeatGitSafe is the active-MR "git safe" answer for a sandbox seat: only
// a state the gateway affirmed, with nothing dirty, stashed or unpushed.
func sandboxSeatGitSafe(live polecat.LiveGitState) bool {
	return live.Source == polecat.GitStateSourceLive && !live.Dirty && live.StashCount == 0 && live.UnpushedCommits == 0
}

// sandboxLostWork asks the lost-work question the nuke gate needs, with the
// same facts and rule check-recovery uses: it returns the bead whose work is
// lost, or "" when nothing is, and an error when the answer is unknown (origin
// or the work bead could not be read), which the caller fails closed on
// (gt-qq6rg.3).
func sandboxLostWork(env sandboxSeatEnv, bd issueShower, rigPath string, fields *beads.AgentFields, hookBead, sourceHint, seatName string, hook polecat.HookBeadDisposition, workTerminal bool) (string, error) {
	facts := polecat.WorkstateFacts{HookBeadSafe: true, AssignedBeadTerminal: workTerminal}
	applyHookDispositionToWorkstateFacts(&facts, hookBead, hook)
	applySandboxLostWorkFacts(&facts, env, bd, rigPath, fields, hookBead, sourceHint, seatName)
	if facts.GitCheckFailed {
		return "", errors.New(facts.GitCheckFailedReason)
	}
	if in := polecat.NewWorkstateInput(facts); in.SandboxWorkLost {
		return in.SandboxWorkBead, nil
	}
	return "", nil
}
