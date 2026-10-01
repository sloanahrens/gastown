package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RoutingModeCheck detects when beads routing.mode is set to "auto", which can
// cause issues to be unexpectedly routed to ~/.beads-planning instead of the
// local .beads directory. This happens because auto mode uses git remote URL
// to detect user role, and non-SSH URLs are interpreted as "contributor" mode.
//
// See: https://github.com/steveyegge/beads/issues/1165
type RoutingModeCheck struct {
	FixableCheck
}

// NewRoutingModeCheck creates a new routing mode check.
func NewRoutingModeCheck() *RoutingModeCheck {
	return &RoutingModeCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "routing-mode",
				CheckDescription: "Check beads routing.mode is explicit (prevents .beads-planning routing)",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks if routing.mode is set to "explicit".
func (c *RoutingModeCheck) Run(ctx *CheckContext) *CheckResult {
	// Check town-level beads config
	townBeadsDir := filepath.Join(ctx.TownRoot, ".beads")
	result := c.checkRoutingMode(ctx, townBeadsDir, "town")
	if result.Status != StatusOK {
		return result
	}

	// Also check rig-level beads if specified
	if ctx.RigName != "" {
		rigBeadsDir := filepath.Join(ctx.RigPath(), ".beads")
		rigResult := c.checkRoutingMode(ctx, rigBeadsDir, fmt.Sprintf("rig '%s'", ctx.RigName))
		if rigResult.Status != StatusOK {
			return rigResult
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Beads routing.mode is explicit",
	}
}

// checkRoutingMode checks the routing mode in a specific beads directory.
func (c *RoutingModeCheck) checkRoutingMode(ctx *CheckContext, beadsDir, location string) *CheckResult {
	// Run bd config get routing.mode. The environment carries
	// PWD=filepath.Dir(beadsDir), which bd reads.
	dir := filepath.Dir(beadsDir)
	mode, err := ctx.bd(dir, append(environWithPWD(dir), "BEADS_DIR="+beadsDir)).ConfigGet("routing.mode")

	// An unset key (bd exits 0 and prints "routing.mode (not set)"; older bd
	// failed saying "not found"/"not set") defaults to "auto".
	if msg := bdOutput(err); mode == "" && (err == nil || strings.Contains(msg, "not found") || strings.Contains(msg, "not set")) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("routing.mode not set at %s (defaults to auto)", location),
			Details: []string{
				"Auto routing mode uses git remote URL to detect user role",
				"Non-SSH URLs (HTTPS or file paths) trigger routing to ~/.beads-planning",
				"This causes mail and issues to be stored in the wrong location",
				"See: https://github.com/steveyegge/beads/issues/1165",
			},
			FixHint: "Run 'gt doctor fix routing-mode' or 'bd config set routing.mode explicit'",
		}
	}
	if err != nil {
		// Other error - report as warning
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Could not check routing.mode at %s: %v", location, bdCause(err)),
		}
	}

	if mode != "explicit" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("routing.mode is '%s' at %s (should be 'explicit')", mode, location),
			Details: []string{
				"Auto routing mode uses git remote URL to detect user role",
				"Non-SSH URLs (HTTPS or file paths) trigger routing to ~/.beads-planning",
				"This causes mail and issues to be stored in the wrong location",
				"See: https://github.com/steveyegge/beads/issues/1165",
			},
			FixHint: "Run 'gt doctor fix routing-mode' or 'bd config set routing.mode explicit'",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("routing.mode is explicit at %s", location),
	}
}

// Fix sets routing.mode to "explicit" in both town and rig beads.
func (c *RoutingModeCheck) Fix(ctx *CheckContext) error {
	// Fix town-level beads
	townBeadsDir := filepath.Join(ctx.TownRoot, ".beads")
	if err := c.setRoutingMode(ctx, townBeadsDir); err != nil {
		return fmt.Errorf("fixing town beads: %w", err)
	}

	// Also fix rig-level beads if specified
	if ctx.RigName != "" {
		rigBeadsDir := filepath.Join(ctx.RigPath(), ".beads")
		if err := c.setRoutingMode(ctx, rigBeadsDir); err != nil {
			return fmt.Errorf("fixing rig %s beads: %w", ctx.RigName, err)
		}
	}

	return nil
}

// setRoutingMode sets routing.mode to "explicit" in the specified beads directory.
func (c *RoutingModeCheck) setRoutingMode(ctx *CheckContext, beadsDir string) error {
	dir := filepath.Dir(beadsDir)
	if err := ctx.bd(dir, append(environWithPWD(dir), "BEADS_DIR="+beadsDir)).ConfigSet("routing.mode", "explicit"); err != nil {
		return fmt.Errorf("bd config set failed: %s", bdOutput(err))
	}

	return nil
}
