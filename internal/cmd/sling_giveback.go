package cmd

import (
	"errors"
	"regexp"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The text shapes below are what a sling prints when its sentinel did not
// survive the trip out of the process that made it. Each one must open the
// message or a wrap segment (after ": ") and carries the rest of the shape the
// town prints, so prose that merely contains the phrase does not match. The
// session shape ends the message: it takes the polecat layer's ": <session id>"
// form, the tmux layer's " with healthy agent" form and the bare phrase, and
// nothing that goes on after them. The
// gateway failure has no sentinel to fall back on: gtwrap wraps the transport
// error without one.
var (
	assigneeLockTextShape   = regexp.MustCompile(`(?:^|: )timed out acquiring assignee sling lock (?:for \S+ )?after \d+s \(`)
	sessionRunningTextShape = regexp.MustCompile(`(?:^|: )session already running(?:: \S+| with healthy agent)?$`)
	gatewayDownTextShape    = regexp.MustCompile(`(?:^|: )gtwrap: the bd gateway at \S+ did not answer, so the call did not run: `)
)

// slingGiveBackReason reports whether a failed sling hands back the respawn
// count its spawn charged, and names the kind of failure for the warning a
// give-back prints. A give-back is for a failure that is the seat's
// precondition rather than a death of the bead: the dispatcher defers such a
// bead and slings it again, so a count per attempt would reach the respawn
// limit on the town's own load (gt-cjmt5, gt-8d38p, gt-a4cdcd, gt-wsf3ht).
//
// A sentinel matches however it is wrapped. The text shapes cover a failure
// that crossed a process boundary, where its sentinel does not survive: a
// sling run as a subprocess reports what it printed, and the daemon classifies
// that text. Each shape is anchored (see the variables above) so an error that
// only embeds one of the phrases keeps its count.
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
		assigneeLockTextShape.MatchString(text):
		return "an assignee-lock timeout", true
	case errors.Is(err, polecat.ErrSessionRunning),
		sessionRunningTextShape.MatchString(text):
		return "a session that was already running", true
	case specdispatch.IsSerializationFailure(err):
		return "a Dolt serialization failure", true
	case gatewayDownTextShape.MatchString(text):
		return "a bd gateway that did not answer", true
	}
	return "", false
}
