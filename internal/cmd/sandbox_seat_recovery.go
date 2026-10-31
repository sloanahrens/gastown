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
	// survivingBranch reports the most recent generated polecat branch on
	// origin that encodes issueID, "" when there is none; nil means the shared
	// reader over the rig's repo. It answers a sandbox seat that recorded no
	// branch, whose issue landed, whether a branch for that issue survives
	// (gt-xhnn3).
	survivingBranch func(rigPath, issueID string) (string, error)
}

func (e sandboxSeatEnv) branchOnOrigin(rigPath, branch string) (bool, error) {
	if e.remoteBranch != nil {
		return e.remoteBranch(rigPath, branch)
	}
	return git.NewGitWithDir(filepath.Join(rigPath, ".repo.git"), "").RemoteBranchExists("origin", branch)
}

func (e sandboxSeatEnv) branchForIssue(rigPath, issueID string) (string, error) {
	if e.survivingBranch != nil {
		return e.survivingBranch(rigPath, issueID)
	}
	return polecat.SurvivingBranchForIssue(rigPath, issueID)
}

// applySandboxLostWorkFacts gives the verdict what it needs to tell lost work
// from a finished seat when the sandbox is gone: the bead the seat was working
// (its hook, else its source issue) and whether anything survives to continue
// from, a resume_branch on that bead or the branch itself on origin, read from
// the remote and never from the stale host worktree (gt-qq6rg.3). A remote that
// cannot be asked leaves the git state unknown rather than claiming either.
func applySandboxLostWorkFacts(facts *polecat.WorkstateFacts, env sandboxSeatEnv, bd issueShower, rigPath string, fields *beads.AgentFields, hookBead, sourceHint string) {
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
			// A seat that recorded no branch names nothing to ask origin
			// about. It is judged by its issue instead, with the manager's
			// rule and its answer: a closed issue carrying a landing record
			// with no branch for it left on origin is finished, and every
			// other answer is today's refusal (gt-xhnn3).
			if reason := polecat.SandboxSeatLandedWithoutBranch(issue, workBead, func(issueID string) (string, error) {
				return env.branchForIssue(rigPath, issueID)
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

// sandboxLostWork asks the lost-work question the nuke gate needs, with the
// same facts and rule check-recovery uses: it returns the bead whose work is
// lost, or "" when nothing is, and an error when the answer is unknown (origin
// or the work bead could not be read), which the caller fails closed on
// (gt-qq6rg.3).
func sandboxLostWork(env sandboxSeatEnv, bd issueShower, rigPath string, fields *beads.AgentFields, hookBead, sourceHint string, hook polecat.HookBeadDisposition, workTerminal bool) (string, error) {
	facts := polecat.WorkstateFacts{HookBeadSafe: true, AssignedBeadTerminal: workTerminal}
	applyHookDispositionToWorkstateFacts(&facts, hookBead, hook)
	applySandboxLostWorkFacts(&facts, env, bd, rigPath, fields, hookBead, sourceHint)
	if facts.GitCheckFailed {
		return "", errors.New(facts.GitCheckFailedReason)
	}
	if in := polecat.NewWorkstateInput(facts); in.SandboxWorkLost {
		return in.SandboxWorkBead, nil
	}
	return "", nil
}
