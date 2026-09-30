package convoy

import (
	"fmt"
	"strings"
)

// Convoy lifecycle statuses.
const (
	StatusOpen           = "open"
	StatusClosed         = "closed"
	StatusStagedReady    = "staged_ready"
	StatusStagedWarnings = "staged_warnings"

	// TrackedStatusUnknown is the status of a tracked dependency whose status
	// could not be resolved: typically a cross-rig bead whose rig DB is
	// missing, parked, or unroutable from the town root. It is distinct from
	// "open" so auto-close does not mistake it for pending work and
	// `gt convoy status` can label it clearly. (gt-bs6 / GH#2786)
	TrackedStatusUnknown = "unknown"
)

// NormalizeStatus lower-cases and trims a convoy status.
func NormalizeStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

// EnsureKnownStatus returns an error unless status is a convoy lifecycle
// status.
func EnsureKnownStatus(status string) error {
	switch NormalizeStatus(status) {
	case StatusOpen, StatusClosed, StatusStagedReady, StatusStagedWarnings:
		return nil
	default:
		return fmt.Errorf(
			"unsupported convoy status %q (expected %q, %q, %q, or %q)",
			status,
			StatusOpen,
			StatusClosed,
			StatusStagedReady,
			StatusStagedWarnings,
		)
	}
}
