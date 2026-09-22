package doctor

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/slot"
)

// SlotDebrisCheck reports the container-gate debris that used to block every
// container-backed suite town-wide: gate containers older than the staleness
// window with no live ryuk reaper, and the owner files dead suites left behind
// (gt-ul1k). --fix reaps them.
//
// It runs the same slot.Reap a dry run would, so the warning names exactly
// what --fix removes; nothing is classified twice by two code paths that could
// disagree.
type SlotDebrisCheck struct {
	FixableCheck
}

// NewSlotDebrisCheck creates a new container-gate debris check.
func NewSlotDebrisCheck() *SlotDebrisCheck {
	return &SlotDebrisCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "slot-debris",
				CheckDescription: "Detect stale container-gate containers and owner files",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run lists the debris without removing anything. An unreadable container
// listing is StatusSkipped, never a clean pass: the check could not see the
// containers, which is not the same as there being none.
func (c *SlotDebrisCheck) Run(ctx *CheckContext) *CheckResult {
	report, err := slot.Reap(ctx.TownRoot, slot.ReapOptions{DryRun: true})
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not list the gate's containers",
			Details: []string{err.Error()},
		}
	}

	if len(report.Debris) == 0 && len(report.OwnerFiles) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("no gate container older than %s and no stale owner files", report.OlderThan),
		}
	}

	details := make([]string, 0, len(report.Debris)+len(report.OwnerFiles))
	for _, verdict := range report.Debris {
		details = append(details, fmt.Sprintf("%s — %s; labels: %s",
			verdict.Container.Display(), verdict.Reason, verdict.Container.LabelSummary()))
	}
	for _, file := range report.OwnerFiles {
		details = append(details, fmt.Sprintf("owner file for slot %d names pid %d, which holds no flock", file.Slot, file.PID))
	}
	if len(report.Kept) > 0 {
		details = append(details, fmt.Sprintf("(%d container(s) kept: still young enough to be a running suite)", len(report.Kept)))
	}

	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("%d stale gate container(s), %d stale owner file(s) — blocking nothing, but worth removing",
			len(report.Debris), len(report.OwnerFiles)),
		Details: details,
		FixHint: "run 'gt doctor --check slot-debris --fix' to remove them",
	}
}

// Fix reaps the debris. It re-runs the classification rather than trusting the
// listing Run cached: between the two calls a suite may have started, and
// removing a container whose suite is now live would break it.
func (c *SlotDebrisCheck) Fix(ctx *CheckContext) error {
	report, err := slot.Reap(ctx.TownRoot, slot.ReapOptions{})
	if err != nil {
		return fmt.Errorf("reaping gate debris: %w", err)
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("reaping gate debris: %s", strings.Join(report.Failed, "; "))
	}
	return nil
}
