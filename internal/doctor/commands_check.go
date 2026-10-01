package doctor

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/templates"
)

// CommandsCheck validates that town-level .claude/commands/ is provisioned.
// All agents inherit these via Claude's directory traversal - no per-workspace copies needed.
type CommandsCheck struct {
	FixableCheck
	townRoot        string   // Cached for Fix
	missingCommands []string // Cached during Run for use in Fix
	staleCommands   []string // Cached during Run for use in Fix
}

// NewCommandsCheck creates a new commands check.
func NewCommandsCheck() *CommandsCheck {
	return &CommandsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "commands-provisioned",
				CheckDescription: "Check .claude/commands/ is provisioned at town level",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks if town-level slash commands are provisioned and current.
func (c *CommandsCheck) Run(ctx *CheckContext) *CheckResult {
	c.townRoot = ctx.TownRoot
	c.missingCommands = nil
	c.staleCommands = nil

	// Check town-level commands
	missing := templates.MissingCommands(ctx.TownRoot)

	// A copy that predates a template edit is as broken as an absent one: the
	// agent reads the old body (gt-w9afa).
	stale := templates.StaleCommands(ctx.TownRoot)

	if len(missing) == 0 && len(stale) == 0 {
		names := templates.CommandNames()
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Town-level slash commands provisioned (%s)", strings.Join(names, ", ")),
		}
	}

	c.missingCommands = missing
	c.staleCommands = stale

	var drift []string
	if len(missing) > 0 {
		drift = append(drift, fmt.Sprintf("missing: %s", strings.Join(missing, ", ")))
	}
	if len(stale) > 0 {
		drift = append(drift, fmt.Sprintf("stale: %s", strings.Join(stale, ", ")))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Town-level slash commands out of date (%s)", strings.Join(drift, "; ")),
		Details: []string{
			fmt.Sprintf("Expected at: %s/.claude/commands/", ctx.TownRoot),
			"A stale copy differs from the command body this binary embeds",
			"All agents inherit town-level commands via directory traversal",
		},
		FixHint: "Run 'gt doctor fix commands-provisioned' to provision missing commands",
	}
}

// Fix provisions town-level slash commands when any are missing or stale.
func (c *CommandsCheck) Fix(ctx *CheckContext) error {
	if len(c.missingCommands) == 0 && len(c.staleCommands) == 0 {
		return nil
	}

	return templates.ProvisionCommands(c.townRoot)
}
