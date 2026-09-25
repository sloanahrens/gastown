package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
			details = append(details, fmt.Sprintf("  %s: update available%s", f.Name, sameVersionClause(ctx.TownRoot, f.Name)))
			needsFix = true
		case "missing":
			details = append(details, fmt.Sprintf("  %s: missing (will reinstall)", f.Name))
			needsFix = true
		case "modified":
			blocked := "preserving a local edit"
			if f.EmbeddedHash != f.InstalledHash {
				blocked = "blocking newer content this binary carries"
			}
			details = append(details, fmt.Sprintf("  %s: hand-edited (%s, NOT delivered)%s", f.Name, blocked, sameVersionClause(ctx.TownRoot, f.Name)))
			modified = append(modified, f.Name)
		case "new":
			details = append(details, fmt.Sprintf("  %s: new formula available", f.Name))
			needsFix = true
		case "untracked":
			details = append(details, fmt.Sprintf("  %s: untracked (will update)%s", f.Name, sameVersionClause(ctx.TownRoot, f.Name)))
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

// sameVersionClause names the version and both sizes for a town-tier copy that
// declares the same version as the embedded one: no version bump announces that
// divergence, so the sizes are the signal a reader has to go on (gt-aydo).
// Empty when the versions differ or either copy does not parse — those findings
// already say everything a version comparison would add.
func sameVersionClause(townRoot, name string) string {
	townData, err := os.ReadFile(filepath.Join(townRoot, ".beads", "formulas", name)) //nolint:gosec // G304: name comes from the embedded formula list
	if err != nil {
		return ""
	}
	embedded, err := formula.GetEmbeddedFormulaContent(name)
	if err != nil {
		return ""
	}

	townF, err := formula.Parse(townData)
	if err != nil {
		return ""
	}
	embeddedF, err := formula.Parse(embedded)
	if err != nil {
		return ""
	}
	if townF.Version != embeddedF.Version {
		return ""
	}

	return fmt.Sprintf("; same version as embedded (%d), different bytes: town %s vs embedded %s",
		townF.Version, exactBytes(len(townData)), exactBytes(len(embedded)))
}

// exactBytes groups a byte count for a detail line ("30,870 B"). The package's
// formatBytes rounds to the nearest unit, which would render two different
// formula sizes as the same string and hide the divergence being reported.
func exactBytes(n int) string {
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return b.String() + " B"
}
