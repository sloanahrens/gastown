package cmd

import (
	"errors"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// slingGiveBackReason reports whether a failed sling hands back the respawn
// count its spawn charged, and names the kind of failure for the warning a
// give-back prints. A give-back is for a failure that is the seat's
// precondition rather than a death of the bead: the dispatcher defers such a
// bead and slings it again, so a count per attempt would reach the respawn
// limit on the town's own load (gt-cjmt5, gt-8d38p, gt-a4cdcd, gt-wsf3ht).
//
// The text matches cover a failure that crossed a process boundary, where its
// sentinel does not survive: a sling run as a subprocess reports what it
// printed, and the daemon classifies that text.
func slingGiveBackReason(err error) (reason string, giveBack bool) {
	if err == nil {
		return "", false
	}
	text := err.Error()
	switch {
	case errors.Is(err, session.ErrImageDrift):
		return "an image-drift deferral", true
	case errors.Is(err, ErrBondLockTimeout):
		return "a bond-lock deferral", true
	case errors.Is(err, ErrAssigneeLockTimeout),
		strings.Contains(text, "timed out acquiring assignee sling lock"):
		return "an assignee-lock timeout", true
	case errors.Is(err, polecat.ErrSessionRunning),
		strings.Contains(text, "session already running"):
		return "a session that was already running", true
	case specdispatch.IsSerializationFailure(err):
		return "a Dolt serialization failure", true
	case strings.Contains(text, "did not answer, so the call did not run"):
		return "a bd gateway that did not answer", true
	}
	return "", false
}
