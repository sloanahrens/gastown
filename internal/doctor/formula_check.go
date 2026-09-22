package doctor

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/formula"
)

// FormulaCheck verifies that embedded formulas are up-to-date.
// It detects outdated formulas (binary updated), missing formulas (user deleted),
// and modified formulas (user customized). Can auto-fix outdated and missing.
type FormulaCheck struct {
	FixableCheck
}

// NewFormulaCheck creates a new formula check.
func NewFormulaCheck() *FormulaCheck {
	return &FormulaCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "formulas",
				CheckDescription: "Check embedded formulas are up-to-date",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks if formulas need updating.
func (c *FormulaCheck) Run(ctx *CheckContext) *CheckResult {
	report, err := formula.CheckFormulaHealth(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Could not check formulas: %v", err),
		}
	}

	// All good
	if report.Outdated == 0 && report.Missing == 0 && report.Modified == 0 && report.New == 0 && report.Untracked == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("%d formulas up-to-date", report.OK),
		}
	}

	// Build details. A hand-edited copy is reported as its own kind of finding:
	// it shortens the message instead of lengthening it, and it leaves the
	// doctor clean only if nobody ever reads it, so say it and say why.
	//
	// "modified" is not fixable here — --fix must not overwrite a user's edits
	// — but it is never benign. While the edited copy stands, the formula
	// content embedded in this binary reaches nobody (gt-dt7r).
	var details []string
	var needsFix bool
	var modified []string

	for _, f := range report.Formulas {
		switch f.Status {
		case "outdated":
			details = append(details, fmt.Sprintf("  %s: update available", f.Name))
			needsFix = true
		case "missing":
			details = append(details, fmt.Sprintf("  %s: missing (will reinstall)", f.Name))
			needsFix = true
		case "modified":
			blocked := "preserving a local edit"
			if f.EmbeddedHash != f.InstalledHash {
				blocked = "blocking newer content this binary carries"
			}
			details = append(details, fmt.Sprintf("  %s: hand-edited (%s, NOT delivered)", f.Name, blocked))
			modified = append(modified, f.Name)
		case "new":
			details = append(details, fmt.Sprintf("  %s: new formula available", f.Name))
			needsFix = true
		case "untracked":
			details = append(details, fmt.Sprintf("  %s: untracked (will update)", f.Name))
			needsFix = true
		}
	}

	// Determine status
	status := StatusOK
	if needsFix || len(modified) > 0 {
		status = StatusWarning
	}

	// Build message
	var parts []string
	if report.Outdated > 0 {
		parts = append(parts, fmt.Sprintf("%d outdated", report.Outdated))
	}
	if report.Missing > 0 {
		parts = append(parts, fmt.Sprintf("%d missing", report.Missing))
	}
	if report.New > 0 {
		parts = append(parts, fmt.Sprintf("%d new", report.New))
	}
	if report.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", report.Untracked))
	}
	if report.Modified > 0 {
		parts = append(parts, fmt.Sprintf("%d hand-edited, embedded content undelivered", report.Modified))
	}

	message := fmt.Sprintf("Formulas: %s", strings.Join(parts, ", "))

	result := &CheckResult{
		Name:    c.Name(),
		Status:  status,
		Message: message,
		Details: details,
	}

	switch {
	case needsFix && len(modified) > 0:
		result.FixHint = "Run 'gt doctor --fix' for the rest; 'gt formula sync --dry-run' names the hand-edited ones"
	case needsFix:
		result.FixHint = "Run 'gt doctor --fix' to update formulas"
	case len(modified) > 0:
		result.FixHint = "Move those edits to an overlay and re-run 'gt formula sync' (see 'gt formula sync --dry-run')"
	}

	return result
}

// Fix updates outdated and missing formulas. Hand-edited copies are left alone:
// overwriting them loses a user's edits, so doctor reports them instead and
// leaves the choice to `gt formula sync --force` or an overlay.
func (c *FormulaCheck) Fix(ctx *CheckContext) error {
	_, err := formula.UpdateFormulas(ctx.TownRoot)
	return err
}
