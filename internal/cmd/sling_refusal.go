package cmd

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/sling"
)

// silenceUsageOnRefusal keeps cobra from printing gt sling's whole usage block
// after a refusal. Usage answers a mistyped invocation; a refusal is the
// command working — the bead is fine and simply not this sling's to take — and
// its own text already carries the reason and the remediation. Cobra's usage
// block otherwise follows the reason and buries it: the last line was all a
// dispatcher log kept of a failed sling, and that line was the usage block
// rather than the cause (gt-fudap).
func silenceUsageOnRefusal(cmd *cobra.Command, err error) {
	if cmd == nil || !isSlingRefusal(err) {
		return
	}
	cmd.SilenceUsage = true
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
