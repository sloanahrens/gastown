package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var hooksSyncDryRun bool

var hooksSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Regenerate all agent hook/settings files",
	Long: `Regenerate the Claude Code settings.json for every role across the workspace:

1. Load base config
2. Apply role override (if exists)
3. Apply rig+role override (if exists)
4. Merge hooks section into existing settings.json (preserving all fields)
5. Write updated settings.json

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
	return hooksSyncRun{
		townRoot: townRoot,
		dryRun:   hooksSyncDryRun,
		home:     hooks.EnvHome(),
		liveFire: liveFireCanary,
		out:      os.Stdout,
		now:      time.Now,
	}.run()
}

// hooksSyncRun is one gt hooks sync: the town, the --dry-run flag, where the
// hook configs live, the canary live-fire probe, where output goes and the
// clock. runHooksSync wires the real ones; unit tests build one with a
// sandbox Home and a scripted probe.
type hooksSyncRun struct {
	townRoot string
	dryRun   bool
	home     hooks.Home
	liveFire func(target hooks.Target) (*doctor.LiveFirePairResult, error)
	out      io.Writer
	now      func() time.Time
}

func (r hooksSyncRun) run() error {
	townRoot := r.townRoot
	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return fmt.Errorf("discovering targets: %w", err)
	}

	if r.dryRun {
		fmt.Fprintln(r.out, "Dry run - showing what would change...")
		fmt.Fprintln(r.out)
	} else {
		fmt.Fprintln(r.out, "Syncing hooks...")
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
	// fan-out. targets[0] is the canary: DiscoverTargets orders its result
	// deterministically, so the same target is probed first every run.
	if !r.dryRun && len(targets) > 0 {
		canary := targets[0]
		targets = targets[1:]

		result, err := syncTargetIn(r.home, canary, false)
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
			fmt.Fprintf(r.out, "  %s %s (%s): %v\n", style.Error.Render("✖"), canary.DisplayKey(), label, err)
			errors++
			failedTargets = append(failedTargets, canary.DisplayKey())
		} else {
			printSyncResult(r.out, townRoot, canary, result, false, &created, &updated, &unchanged)

			pair, pairErr := r.liveFire(canary)
			switch {
			case pairErr != nil:
				fmt.Fprintf(r.out, "  %s canary live-fire skipped for %s: %v\n", style.Warning.Render("~"), canary.DisplayKey(), pairErr)
			case pair.Failed():
				return fmt.Errorf(
					"hooks sync aborted: canary live-fire pair failed against %s (%s) — fan-out stopped before any other target was touched (blocked=%s, allowed=%s)",
					canary.DisplayKey(), canary.Path, pair.Blocked.Verdict, pair.Allowed.Verdict,
				)
			default:
				canaryReport = pairToReport(canary, pair)
				if !pair.Passed() {
					fmt.Fprintf(r.out,
						"  %s canary live-fire pair inconclusive against %s (blocked=%s, allowed=%s) — proceeding without a verified pass\n",
						style.Warning.Render("~"), canary.DisplayKey(), pair.Blocked.Verdict, pair.Allowed.Verdict,
					)
				}
			}

			recordRoleReport(r.home, roleReports, canary)
		}
	}

	for _, target := range targets {
		result, err := syncTargetIn(r.home, target, r.dryRun)
		if err != nil {
			label := "sync error"
			if hooks.IsSettingsIntegrityError(err) {
				label = "integrity violation"
				integrityErrors++
			}
			fmt.Fprintf(r.out,
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

		printSyncResult(r.out, townRoot, target, result, r.dryRun, &created, &updated, &unchanged)
		if !r.dryRun {
			recordRoleReport(r.home, roleReports, target)
		}
	}

	// Summary
	fmt.Fprintln(r.out)
	total := updated + unchanged + created + errors
	if r.dryRun {
		fmt.Fprintf(r.out, "Would sync %d targets (%d to create, %d to update, %d unchanged",
			total, created, updated, unchanged)
	} else {
		fmt.Fprintf(r.out, "Synced %d targets (%d created, %d updated, %d unchanged",
			total, created, updated, unchanged)
	}
	if errors > 0 {
		fmt.Fprintf(r.out, ", %s", style.Error.Render(fmt.Sprintf("%d errors", errors)))
	}
	fmt.Fprintln(r.out, ")")

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
	if !r.dryRun && canaryReport != nil {
		report := &hooks.SyncReport{
			Timestamp: r.now().UTC(),
			Canary:    *canaryReport,
			Roles:     roleReports,
		}
		if err := hooks.WriteSyncReport(townRoot, report); err != nil {
			fmt.Fprintf(r.out, "  %s writing sync report: %v\n", style.Warning.Render("✖"), err)
		}
	}

	return nil
}

// printSyncResult prints one target's sync outcome and updates the running
// created/updated/unchanged counters.
func printSyncResult(w io.Writer, townRoot string, target hooks.Target, result syncResult, dryRun bool, created, updated, unchanged *int) {
	relPath, pathErr := filepath.Rel(townRoot, target.Path)
	if pathErr != nil {
		relPath = target.Path
	}

	switch result {
	case syncCreated:
		if dryRun {
			fmt.Fprintf(w, "  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would create)"))
		} else {
			fmt.Fprintf(w, "  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(created)"))
		}
		*created++
	case syncUpdated:
		if dryRun {
			fmt.Fprintf(w, "  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would update)"))
		} else {
			fmt.Fprintf(w, "  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(updated)"))
		}
		*updated++
	case syncUnchanged:
		fmt.Fprintf(w, "  %s %s %s\n", style.Dim.Render("·"), relPath, style.Dim.Render("(unchanged)"))
		*unchanged++
	}
}

// recordRoleReport records the effective hook set actually computed for
// target into roles, keyed by the target's override key.
func recordRoleReport(home hooks.Home, roles map[string]hooks.SyncReportRole, target hooks.Target) {
	expected, err := home.ComputeExpected(target.Key)
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
	return doctor.RunLiveFirePair(claudePath, target.Path, target.DisplayKey()), nil
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
	return syncTargetIn(hooks.EnvHome(), target, dryRun)
}

// syncTargetIn is syncTarget against the hook configs in home.
func syncTargetIn(home hooks.Home, target hooks.Target, dryRun bool) (syncResult, error) {
	result, err := home.SyncManagedClaudeSettings(target, dryRun)
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
