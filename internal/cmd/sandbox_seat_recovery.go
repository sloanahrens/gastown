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
	// remoteBranch reports whether branch exists on origin, asked of the rig's
	// shared bare repo; nil means ls-remote there.
	remoteBranch func(rigPath, branch string) (bool, error)
}

func (e sandboxSeatEnv) branchOnOrigin(rigPath, branch string) (bool, error) {
	if e.remoteBranch != nil {
		return e.remoteBranch(rigPath, branch)
	}
	return git.NewGitWithDir(filepath.Join(rigPath, ".repo.git"), "").RemoteBranchExists("origin", branch)
}

// applySandboxLostWorkFacts gives the verdict what it needs to tell lost work
// from a finished seat when the sandbox is gone: the bead the seat was working
// (its hook, else its source issue) and whether anything survives to continue
// from, a resume_branch on that bead or the branch itself on origin, read from
// the remote and never from the stale host worktree (gt-qq6rg.3). A remote that
// cannot be asked leaves the git state unknown rather than claiming either.
func applySandboxLostWorkFacts(facts *polecat.WorkstateFacts, env sandboxSeatEnv, bd issueShower, rigPath, branch, hookBead, sourceHint string) {
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
		if branch == "" {
			unknown("the agent bead records no branch to look for on origin")
			return
		}
		onOrigin, err := env.branchOnOrigin(rigPath, branch)
		if err != nil {
			unknown(fmt.Sprintf("could not ask origin whether %s was pushed: %v", branch, err))
			return
		}
		preserved = onOrigin
	}
	facts.SandboxGone = true
	facts.SandboxWorkBead = workBead
	facts.SandboxBranchPreserved = preserved
}

// sandboxSeatGitState turns a sandbox seat's gateway-derived git state into the
// GitState the recovery and nuke checks read. A seat whose sandbox the gateway
// says is gone has nothing left on the host to lose: what was pushed is what
// survived, so it reads clean. A running sandbox, or a gateway that could not be
// asked, is an error, which every caller already treats as "cannot check" and
// fails closed on (gt-qq6rg.2).
func sandboxSeatGitState(live polecat.LiveGitState) (*GitState, error) {
	if live.Source != polecat.GitStateSourceLive {
		reason := live.FailedReason
		if reason == "" {
			reason = "git state of the sandbox seat is unknown"
		}
		return nil, errors.New(reason)
	}
	return &GitState{Clean: true, UncommittedFiles: []string{}}, nil
}

// sandboxSeatGitSafe is the active-MR "git safe" answer for a sandbox seat: only
// a state the gateway affirmed, with nothing dirty, stashed or unpushed.
func sandboxSeatGitSafe(live polecat.LiveGitState) bool {
	return live.Source == polecat.GitStateSourceLive && !live.Dirty && live.StashCount == 0 && live.UnpushedCommits == 0
}

// sandboxBranch is the branch a sandbox seat pushed: the one its agent bead
// recorded, else the host polecat's (a label that is stale only in content).
func sandboxBranch(fields *beads.AgentFields, p *polecat.Polecat) string {
	if fields != nil && fields.Branch != "" {
		return fields.Branch
	}
	return p.Branch
}

// sandboxLostWork asks the lost-work question the nuke gate needs, with the
// same facts and rule check-recovery uses: it returns the bead whose work is
// lost, or "" when nothing is, and an error when the answer is unknown (origin
// or the work bead could not be read), which the caller fails closed on
// (gt-qq6rg.3).
func sandboxLostWork(env sandboxSeatEnv, bd issueShower, rigPath, branch, hookBead, sourceHint string, hook polecat.HookBeadDisposition, workTerminal bool) (string, error) {
	facts := polecat.WorkstateFacts{HookBeadSafe: true, AssignedBeadTerminal: workTerminal}
	applyHookDispositionToWorkstateFacts(&facts, hookBead, hook)
	applySandboxLostWorkFacts(&facts, env, bd, rigPath, branch, hookBead, sourceHint)
	if facts.GitCheckFailed {
		return "", errors.New(facts.GitCheckFailedReason)
	}
	if in := polecat.NewWorkstateInput(facts); in.SandboxWorkLost {
		return in.SandboxWorkBead, nil
	}
	return "", nil
}
