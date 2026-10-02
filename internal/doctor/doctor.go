package doctor

import (
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/ui"
)

// Doctor manages and executes health checks.
type Doctor struct {
	checks []Check
}

// NewDoctor creates a new Doctor with no registered checks.
func NewDoctor() *Doctor {
	return &Doctor{
		checks: make([]Check, 0),
	}
}

// Register adds a check to the doctor's check list.
func (d *Doctor) Register(check Check) {
	d.checks = append(d.checks, check)
}

// RegisterAll adds multiple checks to the doctor's check list.
func (d *Doctor) RegisterAll(checks ...Check) {
	d.checks = append(d.checks, checks...)
}

// Checks returns the list of registered checks.
func (d *Doctor) Checks() []Check {
	return d.checks
}

// Only filters the registered checks down to just those whose Name() is in
// names, preserving registration order. A no-op when names is empty. Unknown
// names simply match nothing — callers that need to detect a typo should
// compare len(names) against len(d.Checks()) after calling this.
func (d *Doctor) Only(names []string) {
	if len(names) == 0 {
		return
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	filtered := make([]Check, 0, len(names))
	for _, c := range d.checks {
		if want[c.Name()] {
			filtered = append(filtered, c)
		}
	}
	d.checks = filtered
}

// categoryGetter interface for checks that provide a category
type categoryGetter interface {
	Category() string
}

// Run executes all registered checks and returns a report.
func (d *Doctor) Run(ctx *CheckContext) *Report {
	return d.RunStreaming(ctx, nil, 0)
}

// RunStreaming executes all registered checks with optional real-time output.
// If w is non-nil, prints each check name as it starts and result when done.
// If slowThreshold > 0, shows hourglass icon for slow checks.
func (d *Doctor) RunStreaming(ctx *CheckContext, w io.Writer, slowThreshold time.Duration) *Report {
	report := NewReport()

	for _, check := range d.checks {
		// Stream: print check name before running
		if w != nil {
			fmt.Fprintf(w, "  %s  %s...", ui.RenderMuted("○"), check.Name())
		}

		start := time.Now()
		result := check.Run(ctx)
		result.Elapsed = time.Since(start)

		// Ensure check name is populated
		if result.Name == "" {
			result.Name = check.Name()
		}
		// Set category from check if available
		if cg, ok := check.(categoryGetter); ok && result.Category == "" {
			result.Category = cg.Category()
		}

		// Stream: overwrite line with result
		if w != nil {
			// Check if slow (hourglass replaces spaces to maintain alignment)
			isSlow := slowThreshold > 0 && result.Elapsed >= slowThreshold
			slowIndicator := "  "
			if isSlow {
				report.Summary.Slow++
				slowIndicator = "⏳"
			}
			fmt.Fprintf(w, "\r  %s%s%s", statusIcon(result.Status), slowIndicator, result.Name)
			if result.Message != "" {
				fmt.Fprintf(w, "%s", ui.RenderMuted(" "+result.Message))
			}
			if isSlow {
				fmt.Fprintf(w, "%s", ui.RenderMuted(" ("+formatDuration(result.Elapsed)+")"))
			}
			fmt.Fprintln(w)
		}

		report.Add(result)
	}

	return report
}

// safeFixCheck calls check.Fix() with panic recovery. If the Fix method panics
// (e.g., due to a Dolt nil pointer dereference propagating in-process — GH#1769),
// the panic is caught and returned as an error instead of crashing gt doctor.
func safeFixCheck(check Check, ctx *CheckContext) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("fix panicked: %v", r)
		}
	}()
	return check.Fix(ctx)
}

// BaseCheck provides a base implementation for checks that don't support auto-fix.
// Embed this in custom checks to get default CanFix() and Fix() implementations.
type BaseCheck struct {
	CheckName        string
	CheckDescription string
	CheckCategory    string // Category for grouping (e.g., CategoryCore)
}

// Category returns the check's category for grouping in output.
func (b *BaseCheck) Category() string {
	return b.CheckCategory
}

// Name returns the check name.
func (b *BaseCheck) Name() string {
	return b.CheckName
}

// Description returns the check description.
func (b *BaseCheck) Description() string {
	return b.CheckDescription
}

// CanFix returns false by default.
func (b *BaseCheck) CanFix() bool {
	return false
}

// Fix returns an error indicating this check cannot be auto-fixed.
func (b *BaseCheck) Fix(ctx *CheckContext) error {
	return ErrCannotFix
}

// FixableCheck provides a base implementation for checks that support auto-fix.
// Embed this and override CanFix() to return true, and implement Fix().
type FixableCheck struct {
	BaseCheck
}

// CanFix returns true for fixable checks.
func (f *FixableCheck) CanFix() bool {
	return true
}
