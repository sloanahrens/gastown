package polecat

import (
	"fmt"
)

// SandboxSeatGitState is the host's git answer for a sandbox seat. It never
// reads the seat's host worktree, because a sandbox polecat edits and commits
// inside its sandbox and the host copy goes stale.
//
//   - While the sandbox runs, or when the gateway errors, the work may live in
//     the sandbox and the host cannot know: the answer is unknown. A gateway
//     blip must never read as a gone sandbox, or a live one would look clean.
//   - Once the gateway affirmatively reports no sandbox of that name, only what
//     was pushed survives. The host has nothing left to measure, so it reports
//     a clean seat. Clean is not "safe to discard": a seat holding a bead with
//     no pushed or resume branch is still lost work, and the recovery verdict
//     turns it into a rework.
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
