package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/plugin"
)

// PatrolHooksWiredCheck verifies that hooks trigger patrol execution.
type PatrolHooksWiredCheck struct {
	FixableCheck
}

// NewPatrolHooksWiredCheck creates a new patrol hooks wired check.
func NewPatrolHooksWiredCheck() *PatrolHooksWiredCheck {
	return &PatrolHooksWiredCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "patrol-hooks-wired",
				CheckDescription: "Check if hooks trigger patrol execution",
				CheckCategory:    CategoryPatrol,
			},
		},
	}
}

// Run checks if patrol hooks are wired.
func (c *PatrolHooksWiredCheck) Run(ctx *CheckContext) *CheckResult {
	daemonConfigPath := config.DaemonPatrolConfigPath(ctx.TownRoot)
	relPath, _ := filepath.Rel(ctx.TownRoot, daemonConfigPath)

	cfg, err := config.LoadDaemonPatrolConfig(daemonConfigPath)
	if errors.Is(err, config.ErrNotFound) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%s not found", relPath),
			FixHint: "Run 'gt doctor --fix' to create default config, or 'gt daemon start' to start the daemon",
		}
	}
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to read daemon config",
			Details: []string{err.Error()},
		}
	}

	if cfg.Patrols.Count() > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Daemon configured with %d patrol(s)", cfg.Patrols.Count()),
		}
	}

	if cfg.Heartbeat != nil && cfg.Heartbeat.Enabled {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Daemon heartbeat enabled (triggers patrols)",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Configure patrols in %s or run 'gt daemon start'", relPath),
		FixHint: "Run 'gt doctor --fix' to create default config",
	}
}

// Fix creates the daemon patrol config with defaults.
func (c *PatrolHooksWiredCheck) Fix(ctx *CheckContext) error {
	return config.EnsureDaemonPatrolConfig(ctx.TownRoot)
}

// PatrolNotStuckCheck detects wisps that have been in_progress too long.
type PatrolNotStuckCheck struct {
	BaseCheck
	stuckThreshold time.Duration
}

// DefaultStuckThreshold is the fallback when no role bead config exists.
// Per ZFC: "Let agents decide thresholds. 'Stuck' is a judgment call."
const DefaultStuckThreshold = 1 * time.Hour

// NewPatrolNotStuckCheck creates a new patrol not stuck check.
func NewPatrolNotStuckCheck() *PatrolNotStuckCheck {
	return &PatrolNotStuckCheck{
		BaseCheck: BaseCheck{
			CheckName:        "patrol-not-stuck",
			CheckDescription: "Check for stuck patrol wisps (>1h in_progress)",
			CheckCategory:    CategoryPatrol,
		},
		stuckThreshold: DefaultStuckThreshold,
	}
}

// Run checks for stuck patrol wisps.
func (c *PatrolNotStuckCheck) Run(ctx *CheckContext) *CheckResult {

	rigs, err := discoverRigs(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to discover rigs",
			Details: []string{err.Error()},
		}
	}

	if len(rigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs configured",
		}
	}

	var stuckWisps []string
	for _, rigName := range rigs {
		rigPath := filepath.Join(ctx.TownRoot, rigName)

		// Query Dolt database (the only supported backend).
		stuck, err := c.checkStuckWispsDolt(ctx, rigPath, rigName)
		if err != nil {
			// Dolt query failed — report as error rather than silently skipping.
			stuckWisps = append(stuckWisps, fmt.Sprintf("%s: Dolt query failed: %v", rigName, err))
			continue
		}
		stuckWisps = append(stuckWisps, stuck...)
	}

	thresholdStr := c.stuckThreshold.String()
	if len(stuckWisps) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d stuck patrol wisp(s) found (>%s)", len(stuckWisps), thresholdStr),
			Details: stuckWisps,
			FixHint: "Manual review required - wisps may need to be burned or sessions restarted",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No stuck patrol wisps found",
	}
}

// checkStuckWispsDolt queries the Dolt database for stuck wisps using bd sql.
// Returns an error if the query fails (caller should fall back to JSONL).
func (c *PatrolNotStuckCheck) checkStuckWispsDolt(ctx *CheckContext, rigPath string, rigName string) ([]string, error) {
	records, err := runBdSQLCSV(ctx, rigPath, beadsql.InProgressByAge())
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, nil // No results (header only or empty)
	}

	var stuck []string
	// Use UTC for cutoff: Dolt stores timestamps in UTC, and time.Parse
	// without timezone info returns UTC times. Using local time here caused
	// false "future timestamp" alarms every evening PDT (gt-ty4).
	cutoff := time.Now().UTC().Add(-c.stuckThreshold)

	for _, rec := range records[1:] { // Skip CSV header
		if len(rec) < 4 {
			continue
		}
		id := strings.TrimSpace(rec[0])
		title := strings.TrimSpace(rec[1])
		updatedAt := strings.TrimSpace(rec[3])

		t, err := time.Parse("2006-01-02 15:04:05", updatedAt)
		if err != nil {
			// Try RFC3339 as fallback
			t, err = time.Parse(time.RFC3339, updatedAt)
			if err != nil {
				continue
			}
		}

		if !t.IsZero() && t.Before(cutoff) {
			stuck = append(stuck, fmt.Sprintf("%s: %s (%s) - stale since %s UTC",
				rigName, id, title, t.UTC().Format("2006-01-02 15:04")))
		}
	}

	return stuck, nil
}

// PatrolPluginsAccessibleCheck verifies plugin directories exist and are readable.
type PatrolPluginsAccessibleCheck struct {
	FixableCheck
	missingDirs []string
}

// NewPatrolPluginsAccessibleCheck creates a new patrol plugins accessible check.
func NewPatrolPluginsAccessibleCheck() *PatrolPluginsAccessibleCheck {
	return &PatrolPluginsAccessibleCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "patrol-plugins-accessible",
				CheckDescription: "Check if plugin directories exist and are readable",
				CheckCategory:    CategoryPatrol,
			},
		},
	}
}

// Run checks if plugin directories are accessible.
func (c *PatrolPluginsAccessibleCheck) Run(ctx *CheckContext) *CheckResult {
	c.missingDirs = nil

	// Check town-level plugins directory
	townPluginsDir := filepath.Join(ctx.TownRoot, "plugins")
	if _, err := os.Stat(townPluginsDir); os.IsNotExist(err) {
		c.missingDirs = append(c.missingDirs, townPluginsDir)
	}

	// Check rig-level plugins directories
	rigs, err := discoverRigs(ctx.TownRoot)
	if err == nil {
		for _, rigName := range rigs {
			rigPluginsDir := filepath.Join(ctx.TownRoot, rigName, "plugins")
			if _, err := os.Stat(rigPluginsDir); os.IsNotExist(err) {
				c.missingDirs = append(c.missingDirs, rigPluginsDir)
			}
		}
	}

	if len(c.missingDirs) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d plugin directory(ies) missing", len(c.missingDirs)),
			Details: c.missingDirs,
			FixHint: "Run 'gt doctor --fix' to create missing directories",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "All plugin directories accessible",
	}
}

// Fix creates missing plugin directories.
func (c *PatrolPluginsAccessibleCheck) Fix(ctx *CheckContext) error {
	for _, dir := range c.missingDirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// PatrolPluginDriftCheck detects when runtime plugins are out of sync with source.
type PatrolPluginDriftCheck struct {
	FixableCheck
	sourceDir string
	targetDir string
}

// NewPatrolPluginDriftCheck creates a new plugin drift check.
func NewPatrolPluginDriftCheck() *PatrolPluginDriftCheck {
	return &PatrolPluginDriftCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "patrol-plugin-drift",
				CheckDescription: "Check if runtime plugins match source repo",
				CheckCategory:    CategoryPatrol,
			},
		},
	}
}

// Run checks for plugin drift between source and runtime.
func (c *PatrolPluginDriftCheck) Run(ctx *CheckContext) *CheckResult {
	c.targetDir = filepath.Join(ctx.TownRoot, "plugins")

	src, err := plugin.FindGastownSource(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "cannot verify plugin drift",
			Details: []string{err.Error()},
			FixHint: "Verify the gastown rig checkout exists at <town>/gastown/mayor/rig, or run 'gt plugin sync --source <dir>'",
		}
	}
	sourceDir := src.Dir
	c.sourceDir = sourceDir

	// Skip if source and target are the same directory
	srcAbs, _ := filepath.Abs(sourceDir)
	tgtAbs, _ := filepath.Abs(c.targetDir)
	if srcAbs == tgtAbs {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Source and runtime are same directory",
		}
	}

	report, err := plugin.DetectDrift(sourceDir, c.targetDir)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to check plugin drift",
			Details: []string{err.Error()},
		}
	}

	if !report.HasDrift() {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Runtime plugins match source",
		}
	}

	var details []string
	for _, d := range report.Drifted {
		details = append(details, fmt.Sprintf("%s: content differs", d.Name))
	}
	for _, name := range report.Missing {
		details = append(details, fmt.Sprintf("%s: missing from runtime", name))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d plugin(s) out of sync", len(report.Drifted)+len(report.Missing)),
		Details: details,
		FixHint: "Run 'gt plugin sync' to update runtime plugins",
	}
}

// Fix syncs plugins from source to runtime.
func (c *PatrolPluginDriftCheck) Fix(ctx *CheckContext) error {
	if c.sourceDir == "" || c.targetDir == "" {
		return fmt.Errorf("drift check did not run; cannot fix")
	}
	result, err := plugin.SyncPlugins(c.sourceDir, c.targetDir, false)
	if err != nil {
		return err
	}
	if len(result.Protected) > 0 {
		return fmt.Errorf("left %d plugin(s) with runtime edits untouched; run 'gt plugin sync' to see them, --force to discard", len(result.Protected))
	}
	return nil
}

// discoverRigs finds all registered rigs.
func discoverRigs(townRoot string) ([]string, error) {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, nil // No rigs configured
		}
		return nil, err
	}

	var rigs []string
	for name := range rigsConfig.Rigs {
		rigs = append(rigs, name)
	}
	return rigs, nil
}
