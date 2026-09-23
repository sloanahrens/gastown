package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var hooksSyncDryRun bool

var hooksSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Regenerate all agent hook/settings files",
	Long: `Regenerate hook and settings files for all agents across the workspace.

For Claude agents (settings.json merge):
1. Load base config
2. Apply role override (if exists)
3. Apply rig+role override (if exists)
4. Merge hooks section into existing settings.json (preserving all fields)
5. Write updated settings.json

For template-based agents (OpenCode, Gemini, Copilot, etc.):
1. Resolve the agent configured for each role
2. Compare deployed hook file against current template
3. Overwrite if content differs

Examples:
  gt hooks sync             # Regenerate all hook/settings files
  gt hooks sync --dry-run   # Show what would change without writing`,
	RunE: runHooksSync,
}

func init() {
	hooksCmd.AddCommand(hooksSyncCmd)
	hooksSyncCmd.Flags().BoolVar(&hooksSyncDryRun, "dry-run", false, "Show what would change without writing")
}

func runHooksSync(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return fmt.Errorf("discovering targets: %w", err)
	}

	if hooksSyncDryRun {
		fmt.Println("Dry run - showing what would change...")
		fmt.Println()
	} else {
		fmt.Println("Syncing hooks...")
	}

	updated := 0
	unchanged := 0
	created := 0
	errors := 0
	integrityErrors := 0
	var failedTargets []string
	roleReports := make(map[string]hooks.SyncReportRole)
	var canaryReport *hooks.SyncReportCanary

	// Canary-first live-fire gate (claude-41j.1 D7/D8): before fanning a
	// hooks change out to every target, sync ONE canary settings file and
	// live-fire the blocked+allowed pair against it. A confirmed failure
	// aborts before any other target is touched. This is the exact gap that
	// let a dropped 'if' field deny every polecat's Bash for 7 minutes on
	// 2026-09-10 — no probe existed on the post-sync file to catch it before
	// fan-out. targets[0] is always the mayor target (DiscoverTargets
	// appends it unconditionally, first), giving a deterministic canary.
	if !hooksSyncDryRun && len(targets) > 0 {
		canary := targets[0]
		targets = targets[1:]

		result, err := syncTarget(canary, false)
		if err != nil {
			// A write failure here is a plain sync error, not a live-fire
			// regression — fold it into the ordinary per-target error
			// handling (below) so fan-out to the remaining, unrelated
			// targets still proceeds and the existing fail-closed reporting
			// still fires.
			label := "sync error"
			if hooks.IsSettingsIntegrityError(err) {
				integrityErrors++
				label = "integrity violation"
			}
			fmt.Printf("  %s %s (%s): %v\n", style.Error.Render("✖"), canary.DisplayKey(), label, err)
			errors++
			failedTargets = append(failedTargets, canary.DisplayKey())
		} else {
			printSyncResult(townRoot, canary, result, false, &created, &updated, &unchanged)

			pair, pairErr := liveFireCanary(canary)
			switch {
			case pairErr != nil:
				fmt.Printf("  %s canary live-fire skipped for %s: %v\n", style.Warning.Render("~"), canary.DisplayKey(), pairErr)
			case pair.Failed():
				return fmt.Errorf(
					"hooks sync aborted: canary live-fire pair failed against %s (%s) — fan-out stopped before any other target was touched (blocked=%s, allowed=%s)",
					canary.DisplayKey(), canary.Path, pair.Blocked.Verdict, pair.Allowed.Verdict,
				)
			default:
				canaryReport = pairToReport(canary, pair)
				if !pair.Passed() {
					fmt.Printf(
						"  %s canary live-fire pair inconclusive against %s (blocked=%s, allowed=%s) — proceeding without a verified pass\n",
						style.Warning.Render("~"), canary.DisplayKey(), pair.Blocked.Verdict, pair.Allowed.Verdict,
					)
				}
			}

			recordRoleReport(roleReports, canary)
		}
	}

	for _, target := range targets {
		result, err := syncTarget(target, hooksSyncDryRun)
		if err != nil {
			label := "sync error"
			if hooks.IsSettingsIntegrityError(err) {
				label = "integrity violation"
				integrityErrors++
			}
			fmt.Printf(
				"  %s %s (%s): %v\n",
				style.Error.Render("✖"),
				target.DisplayKey(),
				label,
				err,
			)
			errors++
			failedTargets = append(failedTargets, target.DisplayKey())
			continue
		}

		printSyncResult(townRoot, target, result, hooksSyncDryRun, &created, &updated, &unchanged)
		if !hooksSyncDryRun {
			recordRoleReport(roleReports, target)
		}
	}

	// Sync template-based (non-Claude) agents at each role location.
	// These agents use SyncForRole (content-aware comparison) instead of the
	// JSON merge path used for Claude targets above.
	locations, locErr := hooks.DiscoverRoleLocations(townRoot)
	if locErr != nil {
		fmt.Printf("  %s discovering role locations: %v\n", style.Error.Render("✖"), locErr)
		errors++
	} else {
		for _, loc := range locations {
			rigPath := ""
			if loc.Rig != "" {
				rigPath = filepath.Join(townRoot, loc.Rig)
			}

			// Use ResolveRoleAgentName (not ResolveRoleAgentConfig) so that hooks are
			// installed based on the *configured* agent, not the *resolved* one.
			// ResolveRoleAgentConfig falls back to claude when the agent binary is not
			// found in PATH (e.g., in CI or on a fresh machine), which would silently
			// skip creating opencode/gemini/etc. plugin files.
			agentName, _ := config.ResolveRoleAgentName(loc.Role, townRoot, rigPath)
			if agentName == "" {
				continue
			}

			preset, ok := config.ResolveAgentPreset(agentName, townRoot, rigPath)
			if !ok || preset.HooksDir == "" || preset.HooksSettingsFile == "" {
				continue
			}

			hooksProvider := preset.HooksProvider
			if hooksProvider == "" {
				hooksProvider = string(preset.Name)
			}

			// Claude targets are already handled by DiscoverTargets + syncTarget above.
			if hooksProvider == "claude" {
				continue
			}

			useSettingsDir := preset.HooksUseSettingsDir

			// Determine sync targets.
			// - Town-level roles (mayor, deacon): the role dir IS the working directory.
			// - Rig roles with useSettingsDir: one shared file in the role parent.
			// - Rig roles without useSettingsDir (OpenCode, etc.): need files in each
			//   individual worktree subdirectory.
			var syncDirs []string
			if loc.Rig == "" || useSettingsDir {
				syncDirs = []string{loc.Dir}
			} else {
				syncDirs = hooks.DiscoverWorktrees(loc.Dir)
			}

			for _, dir := range syncDirs {
				targetPath := filepath.Join(dir, preset.HooksDir, preset.HooksSettingsFile)
				relPath, pathErr := filepath.Rel(townRoot, targetPath)
				if pathErr != nil {
					relPath = targetPath
				}

				if hooksSyncDryRun {
					if _, statErr := os.Stat(targetPath); statErr == nil {
						fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would check "+hooksProvider+")"))
					} else {
						fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would create "+hooksProvider+")"))
						created++
					}
					continue
				}

				result, syncErr := hooks.SyncForRole(hooksProvider, dir, dir, loc.Role,
					preset.HooksDir, preset.HooksSettingsFile, preset.Command, useSettingsDir)
				if syncErr != nil {
					fmt.Printf("  %s %s (%s): %v\n", style.Error.Render("✖"), relPath, hooksProvider, syncErr)
					errors++
					failedTargets = append(failedTargets, relPath)
					continue
				}

				switch result {
				case hooks.SyncCreated:
					fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(created "+hooksProvider+")"))
					created++
				case hooks.SyncUpdated:
					fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(updated "+hooksProvider+")"))
					updated++
				case hooks.SyncUnchanged:
					fmt.Printf("  %s %s %s\n", style.Dim.Render("·"), relPath, style.Dim.Render("(unchanged "+hooksProvider+")"))
					unchanged++
				}
			}
		}
	}

	// Summary
	fmt.Println()
	total := updated + unchanged + created + errors
	if hooksSyncDryRun {
		fmt.Printf("Would sync %d targets (%d to create, %d to update, %d unchanged",
			total, created, updated, unchanged)
	} else {
		fmt.Printf("Synced %d targets (%d created, %d updated, %d unchanged",
			total, created, updated, unchanged)
	}
	if errors > 0 {
		fmt.Printf(", %s", style.Error.Render(fmt.Sprintf("%d errors", errors)))
	}
	fmt.Println(")")

	if errors > 0 {
		if integrityErrors > 0 {
			return fmt.Errorf(
				"hooks sync failed closed: %d integrity violation(s) across %s",
				integrityErrors,
				strings.Join(failedTargets, ", "),
			)
		}
		return fmt.Errorf(
			"hooks sync failed: %d target(s) failed (%s)",
			errors,
			strings.Join(failedTargets, ", "),
		)
	}

	// The sync-report.json is the deployment record: per role, the effective
	// hook set as rendered, plus the canary's live-fire pair result and a
	// timestamp. doctor hooks-sync reads it back and reports Skipped when
	// it's absent — a role count or a role's mail is not proof the hooks
	// that were written actually work end-to-end (claude-41j.1 D7/D8).
	if !hooksSyncDryRun && canaryReport != nil {
		report := &hooks.SyncReport{
			Timestamp: time.Now().UTC(),
			Canary:    *canaryReport,
			Roles:     roleReports,
		}
		if err := hooks.WriteSyncReport(townRoot, report); err != nil {
			fmt.Printf("  %s writing sync report: %v\n", style.Warning.Render("✖"), err)
		}
	}

	return nil
}

// printSyncResult prints one target's sync outcome and updates the running
// created/updated/unchanged counters.
func printSyncResult(townRoot string, target hooks.Target, result syncResult, dryRun bool, created, updated, unchanged *int) {
	relPath, pathErr := filepath.Rel(townRoot, target.Path)
	if pathErr != nil {
		relPath = target.Path
	}

	switch result {
	case syncCreated:
		if dryRun {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would create)"))
		} else {
			fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(created)"))
		}
		*created++
	case syncUpdated:
		if dryRun {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would update)"))
		} else {
			fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(updated)"))
		}
		*updated++
	case syncUnchanged:
		fmt.Printf("  %s %s %s\n", style.Dim.Render("·"), relPath, style.Dim.Render("(unchanged)"))
		*unchanged++
	}
}

// recordRoleReport records the effective hook set actually computed for
// target into roles, keyed by the target's override key.
func recordRoleReport(roles map[string]hooks.SyncReportRole, target hooks.Target) {
	expected, err := hooks.ComputeExpected(target.Key)
	if err != nil {
		return
	}
	roles[target.Key] = hooks.SyncReportRole{
		Role:  target.Role,
		Rig:   target.Rig,
		Path:  target.Path,
		Hooks: *expected,
	}
}

// liveFirePairRunner is overridable so tests never spawn a real claude
// subprocess (production 'gt hooks sync' runs are expected to have claude
// in PATH — this session's own harness is proof of that — so tests must not
// rely on its absence to stay hermetic; they override this var instead).
var liveFirePairRunner = doctor.RunLiveFirePair

// liveFireCanary runs the blocked+allowed live-fire pair against the
// canary's already-synced settings file. Returns a non-nil error only when
// the probe could not even be attempted (claude missing) — that is a soft
// warning to the caller, not grounds to abort the sync, since machines
// without the claude binary installed must still be able to sync hooks.
func liveFireCanary(target hooks.Target) (*doctor.LiveFirePairResult, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude not found in PATH")
	}
	return liveFirePairRunner(claudePath, target.Path, target.DisplayKey()), nil
}

// pairToReport converts a doctor live-fire pair result into the
// hooks.SyncReportCanary shape persisted in sync-report.json.
func pairToReport(target hooks.Target, pair *doctor.LiveFirePairResult) *hooks.SyncReportCanary {
	return &hooks.SyncReportCanary{
		Target:       target.DisplayKey(),
		SettingsPath: target.Path,
		Blocked:      shapeToReport(pair.Blocked),
		Allowed:      shapeToReport(pair.Allowed),
	}
}

func shapeToReport(s doctor.LiveFireShapeResult) hooks.SyncReportShape {
	var v hooks.SyncReportVerdict
	switch s.Verdict {
	case doctor.LiveFirePass:
		v = hooks.SyncReportPass
	case doctor.LiveFireFail:
		v = hooks.SyncReportFail
	default:
		v = hooks.SyncReportInconclusive
	}
	return hooks.SyncReportShape{Verdict: v, Detail: s.Detail}
}

type syncResult int

const (
	syncUnchanged syncResult = iota
	syncUpdated
	syncCreated
)

// syncTarget syncs a single target's .claude/settings.json.
// Uses MarshalSettings/UnmarshalSettings to preserve unknown fields.
func syncTarget(target hooks.Target, dryRun bool) (syncResult, error) {
	result, err := hooks.SyncManagedClaudeSettings(target, dryRun)
	if err != nil {
		return 0, err
	}

	switch result {
	case hooks.SyncCreated:
		return syncCreated, nil
	case hooks.SyncUpdated:
		return syncUpdated, nil
	case hooks.SyncUnchanged:
		return syncUnchanged, nil
	default:
		return 0, fmt.Errorf("unknown sync result: %d", result)
	}
}
