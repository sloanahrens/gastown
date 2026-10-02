package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/sling"
)

// silenceUsageOnFailure keeps cobra from printing gt sling's whole usage block
// after a failure that is not a mistyped invocation. A refusal is the command
// working — the bead is fine and simply not this sling's to take — and its own
// text already carries the reason and the remediation; a runtime failure has
// its reason in the error. Cobra's usage block otherwise follows the reason and
// buries it, and an automatic dispatcher that logs only the last line of a
// failed sling then shows no cause at all (gt-fudap, gt-thnbp).
func silenceUsageOnFailure(cmd *cobra.Command, err error) {
	if cmd == nil || err == nil {
		return
	}
	if isSlingRefusal(err) || !isSlingUsageError(err) {
		cmd.SilenceUsage = true
	}
}

// slingUsageError marks a failure that is the command being mistyped — a
// conflicting flag pair, a missing argument, an unknown rig name — so cobra's
// usage block stays under it. Every other failure from a sling that got as far
// as running (the store unreachable, the daemon restarting, a refusal) prints
// its one error line and nothing else, so a caller that keeps only the last
// line of the output reads the reason and not the end of a usage block
// (gt-thnbp).
type slingUsageError struct{ err error }

func (e *slingUsageError) Error() string { return e.err.Error() }
func (e *slingUsageError) Unwrap() error { return e.err }

func slingUsageErrorf(format string, a ...any) error {
	return &slingUsageError{err: fmt.Errorf(format, a...)}
}

func isSlingUsageError(err error) bool {
	var u *slingUsageError
	return errors.As(err, &u)
}

// isSlingRefusal reports whether err is one of sling's refusals rather than a
// failed dispatch. A refusal carries either a marker that leads its text — the
// contract automatic dispatchers already read to defer rather than fail a bead
// (internal/dispatch/refusal.go) — or, for the guards that keep their reason
// out of that channel, a sentinel errors.Is finds.
func isSlingRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sling.ErrRigUnavailable) || errors.Is(err, errSlingDuplicateContent) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, dispatch.SlingRefusalMarker) ||
		strings.Contains(msg, dispatch.ReslingRefusalMarker)
}
