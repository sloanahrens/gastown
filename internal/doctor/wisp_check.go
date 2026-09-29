package doctor

import (
	"fmt"
	"path/filepath"
	"time"
)

// WispGCCheck reports wisps that bd's age gc would collect: open wisps idle
// longer than the threshold (see beads.WispGCCandidates). It is report-only
// on purpose. `bd mol wisp gc` in age mode deletes without --force, and an
// open merge-request wisp queued past the threshold is one of its
// candidates, so --fix must never run it: deciding to delete open wisps is
// an operator's call (gt-22hdp.13 review C1; the gt-4okk failure class).
type WispGCCheck struct {
	BaseCheck
	threshold time.Duration
}

// NewWispGCCheck creates a new wisp GC check with 1 hour threshold.
func NewWispGCCheck() *WispGCCheck {
	return &WispGCCheck{
		BaseCheck: BaseCheck{
			CheckName:        "wisp-gc",
			CheckDescription: "Report wisps bd's age gc would collect (>1h idle)",
			CheckCategory:    CategoryCleanup,
		},
		threshold: 1 * time.Hour,
	}
}

// Run counts, per rig, the wisps bd mol wisp gc would delete.
func (c *WispGCCheck) Run(ctx *CheckContext) *CheckResult {
	rigs, err := discoverRigs(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to discover rigs",
			Details: []string{err.Error()},
		}
	}

	if len(rigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs configured",
		}
	}

	var details []string
	totalAbandoned := 0

	for _, rigName := range rigs {
		rigPath := filepath.Join(ctx.TownRoot, rigName)
		count := c.countAbandonedWisps(ctx, rigPath)
		if count > 0 {
			totalAbandoned += count
			details = append(details, fmt.Sprintf("%s: %d abandoned wisp(s)", rigName, count))
		}
	}

	if totalAbandoned > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d abandoned wisp(s) found (>%s idle)", totalAbandoned, c.threshold),
			Details: details,
			FixHint: "Review with 'bd mol wisp gc --dry-run' in the rig; doctor does not delete open wisps (queued merge requests are among them)",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No abandoned wisps found",
	}
}

// countAbandonedWisps is how many wisps bd's age gc would delete in a rig,
// asked of bd itself (--dry-run) so the report and gc agree.
func (c *WispGCCheck) countAbandonedWisps(ctx *CheckContext, rigPath string) int {
	ids, err := ctx.bd(rigPath, nil).WispGCCandidates(c.threshold)
	if err != nil {
		// Dolt is the only supported backend — no wisps table means 0 abandoned wisps.
		return 0
	}
	return len(ids)
}
