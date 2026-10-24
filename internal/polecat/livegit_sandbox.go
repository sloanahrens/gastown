package polecat

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// IsSandboxSeatAgent reports whether agent is the agent of an openshell seat in
// ts's polecat_pool.seats. A polecat that records no agent, or an agent with no
// seat, is not a sandbox seat. This is the lookup the stale-hook reap does for
// its own question; the git-state callers share it through here.
func IsSandboxSeatAgent(ts *config.TownSettings, agent string) (bool, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return false, nil
	}
	seats, err := config.Seats(ts, nil)
	if err != nil {
		return false, fmt.Errorf("reading polecat_pool.seats: %w", err)
	}
	for _, s := range seats {
		if s.Agent == agent {
			return s.Backend == config.BackendOpenShell, nil
		}
	}
	return false, nil
}

// SandboxSeatGitState is the host's git answer for a sandbox seat, which never
// reads the seat's host worktree: a sandbox polecat edits and commits inside
// its sandbox, so the host copy is a leftover that reports commits the seat
// has since replaced and misses the ones it made (the idle `capable` seat that
// showed attempt 1's head unpushed while origin held attempt 2's, gt-zz58g).
//
//   - While the sandbox runs, or when the gateway could not be asked or did not
//     answer (existsErr), the work may live in it and the host cannot know: the
//     answer is unknown, which every caller already fails closed on. A gateway
//     blip must never read as a gone sandbox, or a nuke or recovery would treat
//     a live one as clean and lose its unpushed work.
//   - Once the gateway AFFIRMATIVELY reports no sandbox of that name (exists
//     false, existsErr nil: sandbox.Exists, the reaper's rule), what survived is
//     what was pushed. The pushed branch and the bead's recorded branch decide
//     whether work was lost; the host has nothing left to measure, so it
//     reports a clean seat. That clean answer is not "safe to discard": a seat
//     still holding a bead with no pushed or resume branch is lost work, which
//     the recovery verdict (gt-qq6rg) turns back into a rework.
//
// branch is the host worktree's branch name, kept as a label only.
func SandboxSeatGitState(branch string, exists bool, existsErr error, sandboxName string) LiveGitState {
	switch {
	case existsErr != nil:
		return LiveGitState{
			Branch:       branch,
			Source:       GitStateSourceUnknown,
			FailedReason: fmt.Sprintf("could not ask the gateway whether sandbox %s exists: %v", sandboxName, existsErr),
		}
	case exists:
		return LiveGitState{
			Branch:       branch,
			Source:       GitStateSourceUnknown,
			FailedReason: fmt.Sprintf("git state lives in the running sandbox %s", sandboxName),
		}
	}
	return LiveGitState{Branch: branch, Source: GitStateSourceLive}
}
