package cmd

import (
	"errors"

	"github.com/steveyegge/gastown/internal/polecat"
)

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
