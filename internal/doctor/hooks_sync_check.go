package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hooks"
)

// HooksSyncCheck verifies all hook/settings files match what gt hooks sync would generate.
type HooksSyncCheck struct {
	FixableCheck
	outOfSync []hooks.Target

	// computeExpected is the hooks config a Claude target should carry; nil
	// is hooks.ComputeExpected, which reads the base and overrides under ~/.gt.
	computeExpected func(target string) (*hooks.HooksConfig, error)
}

func (c *HooksSyncCheck) expectedFor(target string) (*hooks.HooksConfig, error) {
	if c.computeExpected != nil {
		return c.computeExpected(target)
	}
	return hooks.ComputeExpected(target)
}

// NewHooksSyncCheck creates a new hooks sync validation check.
func NewHooksSyncCheck() *HooksSyncCheck {
	return &HooksSyncCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "hooks-sync",
				CheckDescription: "Verify hooks settings.json files are in sync",
				CheckCategory:    CategoryHooks,
			},
		},
	}
}

// Run checks all managed hook/settings files for sync status.
func (c *HooksSyncCheck) Run(ctx *CheckContext) *CheckResult {
	c.outOfSync = nil

	var details []string
	totalTargets := 0

	// Claude targets — use base+override merge system via DiscoverTargets.
	targets, err := hooks.DiscoverTargets(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Failed to discover targets: %v", err),
			Category: c.Category(),
		}
	}

	for _, target := range targets {
		totalTargets++

		expected, err := c.expectedFor(target.Key)
		if err != nil {
			details = append(details, fmt.Sprintf("%s: error computing expected: %v", target.DisplayKey(), err))
			continue
		}

		current, err := hooks.LoadSettings(target.Path)
		if err != nil {
			details = append(details, fmt.Sprintf("%s: error loading: %v", target.DisplayKey(), err))
			continue
		}

		_, statErr := os.Stat(target.Path)
		fileExists := statErr == nil

		if !fileExists || !hooks.HooksEqual(expected, &current.Hooks) || !hooks.HasClaudePromptDefaults(current) {
			c.outOfSync = append(c.outOfSync, target)
			if !fileExists {
				details = append(details, fmt.Sprintf("%s: missing", target.DisplayKey()))
			} else if !hooks.HasClaudePromptDefaults(current) {
				details = append(details, fmt.Sprintf("%s: missing Claude prompt defaults", target.DisplayKey()))
			} else {
				details = append(details, fmt.Sprintf("%s: out of sync", target.DisplayKey()))
			}
		}
	}

	outOfSyncCount := len(c.outOfSync)
	if outOfSyncCount > 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  fmt.Sprintf("%d target(s) out of sync", outOfSyncCount),
			Details:  details,
			FixHint:  "Run 'gt doctor fix hooks-sync' to regenerate settings files",
			Category: c.Category(),
		}
	}

	// Files matching what ComputeExpected would generate is necessary but
	// not sufficient: it proves the bytes are right, not that the hooks
	// they encode actually work end-to-end. sync-report.json is the
	// deployment record 'gt hooks sync' writes only after its canary
	// live-fire pair ran against a real settings file (claude-41j.1
	// D7/D8) — a role count or a role's mail is not that record. Its
	// absence means "in sync" here proves nothing about whether that was
	// ever verified, so report Skipped rather than OK.
	report, reportErr := hooks.ReadSyncReport(ctx.TownRoot)
	if reportErr != nil {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusSkipped,
			Message:  "unknown: no hooks sync report found — run 'gt hooks sync' to record a live-fire-verified deployment",
			Category: c.Category(),
		}
	}

	if !report.Canary.Passed() {
		return &CheckResult{
			Name:   c.Name(),
			Status: StatusWarning,
			Message: fmt.Sprintf(
				"%d hook targets match, but the last sync's canary live-fire pair was not a verified pass (blocked=%s, allowed=%s, canary=%s at %s)",
				totalTargets, report.Canary.Blocked.Verdict, report.Canary.Allowed.Verdict, report.Canary.Target, report.Timestamp.Format(time.RFC3339),
			),
			FixHint:  "Run 'gt doctor --live-fire' to investigate, then 'gt hooks sync' again once the pair passes",
			Category: c.Category(),
		}
	}

	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusOK,
		Message:  fmt.Sprintf("All %d hook targets in sync (last verified sync: %s)", totalTargets, report.Timestamp.Format(time.RFC3339)),
		Category: c.Category(),
	}
}

// Fix brings all out-of-sync targets back into sync.
func (c *HooksSyncCheck) Fix(ctx *CheckContext) error {
	if len(c.outOfSync) == 0 {
		return nil
	}

	var errs []string

	// Fix Claude targets via merge system.
	for _, target := range c.outOfSync {
		expected, err := c.expectedFor(target.Key)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target.DisplayKey(), err))
			continue
		}

		current, err := hooks.LoadSettings(target.Path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target.DisplayKey(), err))
			continue
		}

		current.Hooks = *expected

		if current.EnabledPlugins == nil {
			current.EnabledPlugins = make(map[string]bool)
		}
		current.EnabledPlugins["beads@beads-marketplace"] = false

		claudeDir := filepath.Dir(target.Path)
		if err := os.MkdirAll(claudeDir, 0755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: creating dir: %v", target.DisplayKey(), err))
			continue
		}

		data, err := hooks.MarshalSettings(current)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: marshal: %v", target.DisplayKey(), err))
			continue
		}
		data = append(data, '\n')

		if err := os.WriteFile(target.Path, data, 0644); err != nil {
			errs = append(errs, fmt.Sprintf("%s: write: %v", target.DisplayKey(), err))
			continue
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
