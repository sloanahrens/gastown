package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/style"
)

// slingEstimateFirst makes the story-point estimate a hand sling owes a bead
// with no sp-minutes label before the bead is hooked (gt-t9kwfu). It is the
// estimate gt spec estimate makes, run through the same seam, so a hand sling
// never starts work that has no estimate. A bead that already carries one, and
// a dry run, pay nothing. An estimate that makes none refuses the sling, naming
// its cause, and the bead is not hooked.
func (r *slingRun) slingEstimateFirst(townRoot, beadID string, labels []string) error {
	if r.opts.dryRun || hasSPMinutesLabel(labels) {
		return nil
	}
	outcome, err := r.estimateFirst(context.Background(), townRoot, beadID)
	if err != nil {
		return fmt.Errorf("refusing to sling %s: the story-point estimate failed: %w", beadID, err)
	}
	// A run that made no estimate, or left no sp-minutes label behind, leaves the
	// bead unjudged: the sling is refused rather than hooking a bead nobody sized.
	if outcome.Action == "none" || outcome.Rung == "none" || outcome.Minutes <= 0 {
		reason := outcome.Reason
		if reason == "" {
			reason = "the estimate left no sp-minutes label"
		}
		return fmt.Errorf("refusing to sling %s: no story-point estimate: %s", beadID, reason)
	}
	if outcome.LogError != "" {
		fmt.Fprintf(r.out, "%s estimate log: %s\n", style.Warning.Render("Warning:"), outcome.LogError)
	}
	fmt.Fprintf(r.out, "%s Estimated %s: %s %s\n", style.Dim.Render("→"), beadID,
		storypoints.SPLabel(outcome.Points), storypoints.MinutesLabel(outcome.Minutes))
	return nil
}

// hasSPMinutesLabel reports whether labels carry an estimate's pre-rounding
// minutes, sp-minutes:<n>.
func hasSPMinutesLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(strings.TrimSpace(l), "sp-minutes:") {
			return true
		}
	}
	return false
}
