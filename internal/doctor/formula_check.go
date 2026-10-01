package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/formula"
)

// FormulaCheck verifies the town formulas dir holds exactly what this binary
// ships. The binary is canonical (gt-fd2cu.3): every file is classified by
// content hash against the embedded formula and the hash gt recorded when it
// wrote the file.
//
//   - missing, new or outdated copies: warning, --fix writes them.
//   - drifted copies (a hash gt never wrote): error, --fix replaces them. While
//     one stands, the formula this binary carries reaches nobody (gt-dt7r).
//   - files the binary does not embed (orphaned copies of deleted formulas,
//     hand-written formulas, *.bak copies): error. --fix never deletes; each is
//     promoted into gastown source or deleted by an operator.
type FormulaCheck struct {
	FixableCheck
}

// NewFormulaCheck creates a new formula check.
func NewFormulaCheck() *FormulaCheck {
	return &FormulaCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "formulas",
				CheckDescription: "Check town formulas match the formulas this binary ships",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run classifies every file in the town formulas dir.
func (c *FormulaCheck) Run(ctx *CheckContext) *CheckResult {
	plan, err := formula.PlanFormulaSync(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Could not check formulas: %v", err),
		}
	}

	var details, parts []string
	addGroup := func(names []string, label, detail string, withVersion bool) {
		if len(names) == 0 {
			return
		}
		parts = append(parts, fmt.Sprintf("%d %s", len(names), label))
		for _, name := range names {
			line := fmt.Sprintf("  %s: %s", name, detail)
			if withVersion {
				line += sameVersionClause(ctx.TownRoot, name)
			}
			details = append(details, line)
		}
	}
	addGroup(plan.Updated(), "outdated", "update available", true)
	addGroup(plan.Reinstalled(), "missing", "missing (will reinstall)", false)
	addGroup(plan.Installed(), "new", "new formula available", false)
	addGroup(plan.ReplacedDrift(), "drifted", "hash is not one gt wrote (hand-edited or hand-copied); --fix replaces it with the embedded formula", true)
	addGroup(plan.Orphaned(), "no longer shipped", "gt wrote it but this binary no longer embeds it; delete it", false)
	addGroup(plan.Unowned(), "not owned by gt", "not in gastown source and never written by gt; promote it into source or delete it", false)

	if len(parts) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("%d formulas match this binary", plan.UpToDate()),
		}
	}

	status := StatusWarning
	if len(plan.ReplacedDrift())+len(plan.Orphaned())+len(plan.Unowned()) > 0 {
		status = StatusError
	}

	result := &CheckResult{
		Name:    c.Name(),
		Status:  status,
		Message: fmt.Sprintf("Formulas: %s", strings.Join(parts, ", ")),
		Details: details,
	}
	switch {
	case plan.Changed() > 0 && len(plan.Orphaned())+len(plan.Unowned()) > 0:
		result.FixHint = "Run 'gt doctor --fix' to write the embedded formulas; delete the files gt does not own from .beads/formulas/ by hand"
	case plan.Changed() > 0:
		result.FixHint = "Run 'gt doctor --fix' (or 'gt formula sync') to write the embedded formulas"
	default:
		result.FixHint = "Delete the files gt does not own from .beads/formulas/, or promote a still-used one into gastown source"
	}
	return result
}

// Fix writes every embedded formula whose town copy differs. It never deletes
// a file the binary does not embed.
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
