package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// ConvoyRetirementCheck closes the convoy beads left open when convoys were
// retired (gt-gzhin.6). The daemon's convoy feeder and gt sling's auto-convoys
// are deleted, so nothing feeds or completes a convoy any more and an open one
// is work no agent can finish. The beads a convoy tracked are untouched: each
// keeps its own status and holder.
//
// The check is the one-shot the cutover asks for: Run finds the leftovers,
// `gt doctor fix convoy-retirement` closes them, and a later run finds nothing.
type ConvoyRetirementCheck struct {
	FixableCheck

	// open is what Run found, for Fix to close.
	open []string
}

// Convoy bead identity, both markers a convoy can wear. internal/convoy owns
// these today (ConvoyLabel and IsConvoyIssue); they are repeated here because
// gt-gzhin.7 deletes that package and this check outlives it.
const (
	convoyLabel     = "gt:convoy"
	convoyIssueType = "convoy"

	// convoyRetiredReason is recorded on every bead this check closes.
	convoyRetiredReason = "convoys retired"
)

// NewConvoyRetirementCheck creates the retired-convoy one-shot.
func NewConvoyRetirementCheck() *ConvoyRetirementCheck {
	return &ConvoyRetirementCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "convoy-retirement",
				CheckDescription: "Close convoy beads left open by the retired daemon convoy feeder",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// DestructiveFix marks the repair destructive (gt-638go.3): it closes beads.
func (c *ConvoyRetirementCheck) DestructiveFix() bool { return true }

// Run lists the town's open convoy beads and reports them.
func (c *ConvoyRetirementCheck) Run(ctx *CheckContext) *CheckResult {
	c.open = nil
	result := func(status CheckStatus, msg string, fixHint string, details ...string) *CheckResult {
		return &CheckResult{
			Name:     c.Name(),
			Status:   status,
			Message:  msg,
			Details:  details,
			FixHint:  fixHint,
			Category: c.CheckCategory,
		}
	}

	open, err := c.listOpen(ctx)
	if err != nil {
		// A store that would not answer proves nothing: a fix built on the
		// empty list would report a clean town it never read.
		return result(StatusSkipped, fmt.Sprintf("Cannot list convoys: %v", err), "")
	}
	if len(open) == 0 {
		return result(StatusOK, "No open convoys", "")
	}

	c.open = open
	return result(StatusWarning,
		fmt.Sprintf("%d open convoy(s) left by the retired convoy feeder", len(open)),
		"Close them: gt doctor fix convoy-retirement --authorized-by <bead-id>",
		open...)
}

// Fix closes every convoy Run found. The close is forced because a convoy
// whose tracked issues are still open is exactly the leftover this retires —
// the tracked beads keep their own status either way.
func (c *ConvoyRetirementCheck) Fix(ctx *CheckContext) error {
	if len(c.open) == 0 {
		return nil
	}
	return ctx.beadsAt(ctx.TownRoot).ForceCloseWithReason(convoyRetiredReason, c.open...)
}

// listOpen returns the open convoys in the town store, deduplicated: a bead
// wearing both markers is one convoy.
func (c *ConvoyRetirementCheck) listOpen(ctx *CheckContext) ([]string, error) {
	store := ctx.beadsAt(ctx.TownRoot)
	status := string(beads.StatusOpen)

	labeled, err := store.List(beads.ListOptions{Label: convoyLabel, Status: status, Priority: -1})
	if err != nil {
		return nil, err
	}
	typed, err := store.List(beads.ListOptions{IssueType: convoyIssueType, Status: status, Priority: -1})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(labeled))
	open := make([]string, 0, len(labeled)+len(typed))
	for _, issue := range append(labeled, typed...) {
		if seen[issue.ID] {
			continue
		}
		seen[issue.ID] = true
		open = append(open, issue.ID)
	}
	return open, nil
}
