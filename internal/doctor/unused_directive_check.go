package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
)

// UnusedDirectiveCheck reports directive files whose name targets no role, so
// config.LoadRoleDirective never renders them.
//
// The check embeds BaseCheck and so cannot be auto-fixed: Fix would have to
// delete files the operator wrote, and a misnamed keeper (refiner.md holding
// the refinery's policy) is indistinguishable from junk. The reported hint
// sends the operator to a rename instead.
type UnusedDirectiveCheck struct {
	BaseCheck
}

// NewUnusedDirectiveCheck creates a new check for unused directive files.
func NewUnusedDirectiveCheck() *UnusedDirectiveCheck {
	return &UnusedDirectiveCheck{
		BaseCheck: BaseCheck{
			CheckName:        "unused-directives",
			CheckDescription: "Detect directive files not named for an agent role",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run reports every directive file that no role loads.
func (c *UnusedDirectiveCheck) Run(ctx *CheckContext) *CheckResult {
	files, err := config.ScanDirectiveFiles(ctx.TownRoot, ctx.RigName)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not scan directive files",
			Details: []string{err.Error()},
		}
	}

	var details []string
	for _, f := range files {
		if f.Unused() {
			details = append(details, fmt.Sprintf("Unused: %s (no role named %q)", f.Path, f.Role))
		}
	}

	if len(details) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No unused directive files found",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d directive file(s) reach no role", len(details)),
		Details: details,
		FixHint: fmt.Sprintf(
			"Rename each file to a role name, or move shared policy into %s.md — this check never deletes files",
			config.SharedDirectiveName),
	}
}
