package doctor

import (
	"fmt"
	"strings"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// LifecycleDefaultsCheck detects missing lifecycle patrol entries in the daemon
// patrol config and auto-populates them with sensible defaults.
//
// Existing towns that upgraded may be missing entries for newer Dogs
// (Compactor, JSONL backup, scheduled maintenance, etc.). This check
// invokes the same EnsureLifecycleDefaults logic used by gt init/gt up.
type LifecycleDefaultsCheck struct {
	FixableCheck
	missing []string
}

// NewLifecycleDefaultsCheck creates a new lifecycle defaults check.
func NewLifecycleDefaultsCheck() *LifecycleDefaultsCheck {
	return &LifecycleDefaultsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "lifecycle-defaults",
				CheckDescription: "Check the daemon patrol config has all lifecycle patrol entries",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks for missing lifecycle patrol entries in the daemon patrol config.
// Messages name the place the config lives (settings/config.json's "daemon"
// section once gt config migrate has run, mayor/daemon.json before it).
func (c *LifecycleDefaultsCheck) Run(ctx *CheckContext) *CheckResult {
	c.missing = nil
	where := agentconfig.SourcePathRel(ctx.TownRoot, agentconfig.DaemonPatrolConfigPath(ctx.TownRoot))

	config, err := daemon.ReadPatrolConfig(ctx.TownRoot)
	if err != nil {
		// Never offer gt doctor fix lifecycle-defaults here: EnsureLifecycleConfigFile refuses to
		// rewrite a file that does not parse (gt-fcxe9.10).
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: err.Error(),
			FixHint: "Fix " + where + " by hand; gt never rewrites a file it cannot parse",
		}
	}
	if config == nil {
		// No daemon patrol config at all — EnsureLifecycleConfigFile handles creation.
		// Report as warning so gt doctor fix lifecycle-defaults can create it.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: where + " not found",
			FixHint: "Run 'gt doctor fix lifecycle-defaults' to create with defaults",
		}
	}

	if config.Patrols == nil {
		c.missing = []string{"patrols (entire section)"}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: where + " missing patrols section",
			FixHint: "Run 'gt doctor fix lifecycle-defaults' to populate defaults",
		}
	}

	// The same set EnsureLifecycleDefaults fills, so Fix clears what Run reports.
	c.missing = daemon.MissingLifecyclePatrols(config.Patrols)

	if len(c.missing) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All lifecycle patrols configured",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Missing %d lifecycle patrol(s): %s", len(c.missing), strings.Join(c.missing, ", ")),
		FixHint: "Run 'gt doctor fix lifecycle-defaults' to populate defaults",
	}
}

// Fix populates missing lifecycle patrol entries with defaults.
func (c *LifecycleDefaultsCheck) Fix(ctx *CheckContext) error {
	return daemon.EnsureLifecycleConfigFile(ctx.TownRoot)
}
