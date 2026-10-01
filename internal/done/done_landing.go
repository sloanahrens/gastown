package done

import (
	"errors"
	"fmt"
)

// gt done exit statuses for work that was not submitted (G5-01). Each is
// returned the moment its stage fails, before the Witness hears anything, so
// the session stays up and the polecat fixes what the message names and runs
// gt done again. A caller never mistakes a dropped submission for a landed one.
const (
	// doneExitPushFailed: the push command failed and origin does not have
	// the commit.
	doneExitPushFailed = 10
	// doneExitPushUnverified: origin is not at the commit gt done would
	// declare, although the push command succeeded.
	doneExitPushUnverified = 11
	// doneExitReadyFailed: the branch is on origin but the work bead could not
	// be marked ready to land (label, READY TO LAND note, or read-back).
	doneExitReadyFailed = 12
	// doneExitCloseFailed: a completion with no code could not close its bead.
	doneExitCloseFailed = 13
	// doneExitRebaseConflict: the rebase onto the target conflicted and was
	// aborted; the branch is as it was.
	doneExitRebaseConflict = 14
	// doneExitGateFailed: the local gate ran and failed; nothing was pushed.
	doneExitGateFailed = 15
	// doneExitGateUnavailable: the local gate could not run (no gate
	// configured, a tool missing, golangci-lint's lock never released). It is
	// not a verdict on the change; nothing was pushed.
	doneExitGateUnavailable = 16
)

// doneExit builds the coded error for an unsubmitted outcome. cause may be nil.
func doneExit(code int, msg string, cause error) error {
	err := errors.New("gt done: work not submitted: " + msg)
	if cause != nil {
		err = fmt.Errorf("gt done: work not submitted: %s: %w", msg, cause)
	}
	return &ExitCodeError{Code: code, Err: err}
}
