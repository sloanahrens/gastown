package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
)

// ConfigLayoutCheck reports which config layout the town is on
// (gt-y3pgh.7, internal/config/layout.go). The five-file layout still
// loads, so it is a warning, never an error. It has no --fix: the move is
// gt config migrate, run by the operator.
type ConfigLayoutCheck struct {
	BaseCheck
}

// NewConfigLayoutCheck creates the config-layout check.
func NewConfigLayoutCheck() *ConfigLayoutCheck {
	return &ConfigLayoutCheck{
		BaseCheck: BaseCheck{
			CheckName:        "config-layout",
			CheckDescription: "Check that the town config is on two files (mayor/town.json, settings/config.json)",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run reports the layout and, off the two-file one, the files to move.
func (c *ConfigLayoutCheck) Run(ctx *CheckContext) *CheckResult {
	r, err := config.DetectLayout(ctx.TownRoot)
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "Town config does not load; see town-config-parse"}
	}
	switch r.Layout {
	case config.LayoutTwoFile:
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: fmt.Sprintf("Two-file layout (%s, %s)", config.MachineConfigFile, config.OperatorConfigFile)}
	case config.LayoutFiveFile:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Five-file config layout; it still loads, and the two-file layout replaces it",
			Details: r.Left,
			FixHint: "gt config migrate --dry-run, then gt config migrate",
		}
	default:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Partly migrated: %d old file(s) beside their sections; the sections win", len(r.Left)),
			Details: r.Left,
			FixHint: "gt config migrate (finishes the move; refuses if an old file differs from its section)",
		}
	}
}
