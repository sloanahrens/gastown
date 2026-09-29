package doctor

import (
	"fmt"
	"path/filepath"
	"time"

)

// WispGCCheck detects and cleans orphaned wisps that are older than a threshold.
// Wisps are ephemeral issues (Wisp: true flag) used for patrol cycles and
// operational workflows that shouldn't accumulate.
type WispGCCheck struct {
	FixableCheck
	threshold     time.Duration
	abandonedRigs map[string]int // rig -> count of abandoned wisps
}

// NewWispGCCheck creates a new wisp GC check with 1 hour threshold.
func NewWispGCCheck() *WispGCCheck {
	return &WispGCCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "wisp-gc",
				CheckDescription: "Detect and clean orphaned wisps (>1h old)",
				CheckCategory:    CategoryCleanup,
			},
		},
		threshold:     1 * time.Hour,
		abandonedRigs: make(map[string]int),
	}
}

// Run checks for abandoned wisps in each rig.
func (c *WispGCCheck) Run(ctx *CheckContext) *CheckResult {
	c.abandonedRigs = make(map[string]int)

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
			c.abandonedRigs[rigName] = count
			totalAbandoned += count
			details = append(details, fmt.Sprintf("%s: %d abandoned wisp(s)", rigName, count))
		}
	}

	if totalAbandoned > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d abandoned wisp(s) found (>1h old)", totalAbandoned),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to garbage collect orphaned wisps",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No abandoned wisps found",
	}
}

// countAbandonedWisps counts wisps older than the threshold in a rig.
// Queries the wisps table via bd mol wisp list (Dolt server is required).
func (c *WispGCCheck) countAbandonedWisps(ctx *CheckContext, rigPath string) int {
	// Query wisps table via bd CLI
	wisps, err := ctx.bd(rigPath, nil).MolWispList()
	if err != nil {
		// Dolt is the only supported backend — no wisps table means 0 abandoned wisps.
		return 0
	}

	// Use UTC for cutoff: Dolt stores timestamps in UTC (gt-ty4).
	cutoff := time.Now().UTC().Add(-c.threshold)
	count := 0
	for _, w := range wisps {
		if w.Status == "closed" {
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339, w.UpdatedAt)
		if err != nil {
			continue
		}
		if !updatedAt.IsZero() && updatedAt.Before(cutoff) {
			count++
		}
	}

	return count
}

// Fix runs bd mol wisp gc in each rig with abandoned wisps.
func (c *WispGCCheck) Fix(ctx *CheckContext) error {
	var lastErr error

	for rigName := range c.abandonedRigs {
		rigPath := filepath.Join(ctx.TownRoot, rigName)

		// Run bd mol wisp gc
		if err := ctx.bd(rigPath, nil).GCWisps(); err != nil {
			lastErr = fmt.Errorf("%s: %v (%s)", rigName, bdCause(err), bdOutput(err))
		}
	}

	return lastErr
}
