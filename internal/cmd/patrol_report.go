package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/witness"
)

var (
	patrolReportSummary string
	patrolReportSteps   string
)

var patrolReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Close patrol cycle with summary and start next cycle",
	Long: `Close the current patrol cycle, recording a summary of observations,
then automatically start a new patrol cycle.

This replaces the old squash+new pattern with a single command that:
  1. Closes the current patrol root wisp with the summary
  2. Creates a new patrol wisp for the next cycle

The summary is stored on the patrol root wisp for audit purposes.
The --steps flag records which patrol steps were executed vs skipped,
making shortcutting visible in the ledger.

Examples:
  gt patrol report --summary "All clear, no issues" --steps "heartbeat:OK,inbox-check:OK,health-scan:OK"
  gt patrol report --summary "Dolt latency elevated, filed escalation"`,
	RunE: runPatrolReport,
}

func init() {
	patrolReportCmd.Flags().StringVar(&patrolReportSummary, "summary", "", "Brief summary of patrol observations (required)")
	patrolReportCmd.Flags().StringVar(&patrolReportSteps, "steps", "", "Step audit: comma-separated step:STATUS pairs (e.g., heartbeat:OK,inbox-check:OK)")
	_ = patrolReportCmd.MarkFlagRequired("summary")
}

func runPatrolReport(cmd *cobra.Command, args []string) error {
	// Resolve role
	roleInfo, err := GetRole()
	if err != nil {
		return fmt.Errorf("detecting role: %w", err)
	}

	// A witness may respawn its own session after the report, at a quiet
	// boundary, when its rig opts in (claude-8w7). Flag off: today's report.
	if roleInfo.Role == RoleWitness && roleInfo.Rig != "" {
		if cfg := rig.ResolveWitnessSessionConfig(roleInfo.TownRoot, roleInfo.Rig); cfg != nil && cfg.CycleSessionAtIdleCap {
			p := witnessCycleParamsFor(roleInfo.TownRoot, roleInfo.Rig, true, cfg.MinCycles(), cfg.MaxCycles())
			_, err := reportAndMaybeCycleWitness(p,
				defaultWitnessCycleDeps(roleInfo, p, patrolReportSummary, patrolReportSteps, os.Stdout))
			return err
		}
	}

	return runPatrolReportFor(os.Stdout, roleInfo, patrolReportSummary, patrolReportSteps, true)
}

// runPatrolReportFor closes roleInfo's active patrol with summary and pours
// the next cycle. It is shared by `gt patrol report` and the refinery's
// unit cycle (completeUnitAndCycle). All output goes to out, never directly
// to os.Stdout, so a caller that respawns its own pane right after can
// capture or discard it. When drainNudges is false, queued nudges are left
// in place for the caller to surface after its own next step, instead of
// being drained and printed here.
func runPatrolReportFor(out io.Writer, roleInfo RoleInfo, summary, steps string, drainNudges bool) error {
	roleName := string(roleInfo.Role)

	// Build config based on role
	cfg, ok := patrolConfigForRole(roleInfo)
	if !ok {
		return fmt.Errorf("unsupported role for patrol report: %q", roleName)
	}

	// Bound the witness's hand-maintained cycle counter before anything else,
	// so no exit path from this command can leave it accumulating. The role
	// template's old stop rule read patrol_count >= 15; a session still running
	// an older rendered prompt would obey it, and a template edit cannot reach
	// that prompt (gt prime writes the role prompt for the *next* spawn). See
	// witness.NormalizePatrolCounter (gt-oabl).
	//
	// Witness only: the deacon keeps a bounded counter with a real handoff rule
	// by design (gt-wdv9), so its file must not be touched here.
	if roleInfo.Role == RoleWitness {
		if changed, err := witness.NormalizePatrolCounter(roleInfo.TownRoot, roleInfo.Rig); err != nil {
			style.PrintWarning("could not bound witness patrol counter: %v", err)
		} else if changed {
			fmt.Fprintf(out, "%s Bounded witness patrol counter (reset patrol_count to 0)\n", style.Success.Render("✓"))
		}
	}

	// Find the active patrol
	patrolID, _, hasPatrol, findErr := findActivePatrol(cfg)
	if findErr != nil {
		return fmt.Errorf("finding active patrol: %w", findErr)
	}
	if !hasPatrol {
		// Re-attach instead of dead-ending. A patrol role that lost its wisp —
		// reaped, or a session that primed without the molecule section, which
		// the hook budget can drop (gt-oabl) — has no way back on its own today:
		// this command returned an error, and the only seed path is `gt patrol
		// new`, which nothing invokes. The agent is then idle with an empty hook
		// and its rig goes unwatched. Seeding here makes the loop's own
		// re-entry command heal the loop, and the cycle below closes and
		// re-spawns it exactly as a normal cycle would.
		//
		// A parked or docked rig is an operator stop, not a lost wisp — respect
		// it, the same way prime's witness path does (IsRigParkedOrDocked).
		if roleInfo.Rig != "" {
			if stopped, reason := IsRigParkedOrDocked(roleInfo.TownRoot, roleInfo.Rig); stopped {
				return fmt.Errorf("no active patrol found for %s and rig %s is %s",
					cfg.RoleName, roleInfo.Rig, reason)
			}
		}

		seeded, seedErr := autoSpawnPatrol(cfg)
		if seedErr != nil && seeded == "" {
			return fmt.Errorf("no active patrol for %s and could not start one: %w", cfg.RoleName, seedErr)
		}
		patrolID = seeded
		if seedErr != nil {
			fmt.Fprintf(os.Stderr, "warning: %s\n", seedErr.Error())
		}
		fmt.Fprintf(out, "%s No active patrol for %s — seeded %s and continuing the cycle\n",
			style.Success.Render("✓"), cfg.RoleName, patrolID)
	}

	// Close the current patrol root with the summary
	b := cfg.Beads
	if b == nil {
		b = beads.New(cfg.BeadsDir)
	}

	// Build step audit checklist. The audit is the anti-shortcut record, so a
	// malformed entry (a bare step id with no ":STATUS") fails the command
	// before anything is closed — a silent default to SKIP would make a full
	// patrol read as "0/N" in the ledger (gt-gvo8m).
	stepAudit, err := buildStepAudit(out, cfg.PatrolMolName, steps)
	if err != nil {
		return fmt.Errorf("invalid --steps: %w", err)
	}

	// Update the description with the patrol summary and step audit
	desc := fmt.Sprintf("Patrol report: %s\n\n%s", summary, stepAudit)
	if err := b.Update(patrolID, beads.UpdateOptions{
		Description: &desc,
	}); err != nil {
		style.PrintWarning("could not update patrol summary: %v", err)
	}

	// Print the step audit for visibility
	fmt.Fprintln(out, stepAudit)

	// Close all descendant wisps first (recursive), then the patrol root.
	// Without this, every patrol cycle leaks ~10 orphan wisps into the DB.
	// If descendants can't be closed, abort so patrol retries next cycle (gt-7lx3).
	closed, closeDescErr := forceCloseDescendants(b, patrolID)
	if closeDescErr != nil {
		return fmt.Errorf("closing descendants of patrol %s (closed %d): %w", patrolID, closed, closeDescErr)
	}

	// Close the patrol root
	if err := b.ForceCloseWithReason("patrol cycle complete: "+summary, patrolID); err != nil {
		return fmt.Errorf("closing patrol %s: %w", patrolID, err)
	}

	fmt.Fprintf(out, "%s Closed patrol %s\n", style.Success.Render("✓"), patrolID)

	// Start next cycle
	newPatrolID, err := autoSpawnPatrol(cfg)
	if err != nil {
		if newPatrolID != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", err.Error())
			fmt.Fprintf(out, "New patrol: %s\n", newPatrolID)
			return nil
		}
		return fmt.Errorf("starting next patrol cycle: %w", err)
	}

	fmt.Fprintf(out, "%s Started new patrol: %s\n", style.Success.Render("✓"), newPatrolID)
	if cfg.RoleName == "deacon" {
		stampDeaconHeartbeatOnReport(cfg.BeadsDir, summary)
	}

	// Surface any nudges queued for this session (gt-saz7a). patrol report
	// closes every patrol cycle regardless of how long the prior gate ran, so
	// it is a step boundary a long-running patrol turn actually passes
	// through — unlike the UserPromptSubmit hook, which only fires between
	// turns and never reaches a session that stays in one turn for hours.
	// The refinery's unit cycle passes drainNudges=false so queued nudges
	// survive its pane respawn instead of being drained and discarded here.
	if drainNudges {
		if drained := drainSessionNudges(roleInfo.TownRoot); len(drained) > 0 {
			_, _ = fmt.Fprint(out, nudge.FormatForInjection(drained))
		}
	}

	return nil
}

func stampDeaconHeartbeatOnReport(townRoot, summary string) {
	paused, _, err := deacon.IsPaused(townRoot)
	if err != nil {
		style.PrintWarning("not stamping deacon heartbeat: pause state unreadable: %v", err)
		return
	}
	if paused {
		return
	}

	action := "patrol report"
	if summary = strings.TrimSpace(summary); summary != "" {
		action += ": " + summary
	}
	if err := syncDeaconHeartbeatStores(townRoot, action); err != nil {
		style.PrintWarning("could not stamp deacon heartbeat: %v", err)
	}
}

// buildStepAudit builds a step checklist from the formula's steps and the
// reported step results. Format:
//
//	Steps: heartbeat OK | inbox-check OK | orphan-cleanup SKIP | ... (14/25)
//
// If stepsFlag is empty, it returns a line indicating the audit was not
// reported. A malformed entry — a bare step id with no ":STATUS" — is an
// error (gt-gvo8m): the audit is the anti-shortcut record of the ledger, so a
// silent default to SKIP would make a fully executed patrol read as "0/N".
func buildStepAudit(out io.Writer, formulaName string, stepsFlag string) (string, error) {
	// Load the formula to get the canonical step list
	content, err := formula.GetEmbeddedFormulaContent(formulaName)
	if err != nil {
		if stepsFlag == "" {
			return "Steps: NOT REPORTED (formula not found)", nil
		}
		// Validate the entries even though the formula is unresolvable: the
		// unvalidated path prints them raw, so it must not accept entries
		// that are malformed (a bare step id has no status, gt-gvo8m).
		if _, err := parseStepResults(stepsFlag); err != nil {
			return "", err
		}
		// Can't validate without the formula, but still show what was reported
		return fmt.Sprintf("Steps: %s (unvalidated — formula not found)", stepsFlag), nil
	}

	f, err := formula.Parse(content)
	if err != nil {
		if stepsFlag == "" {
			return "Steps: NOT REPORTED (formula parse error)", nil
		}
		// Same: validate before printing the raw entries (gt-gvo8m).
		if _, err := parseStepResults(stepsFlag); err != nil {
			return "", err
		}
		return fmt.Sprintf("Steps: %s (unvalidated — formula parse error)", stepsFlag), nil
	}

	allStepIDs := f.GetAllIDs()
	if len(allStepIDs) == 0 {
		return "", nil
	}

	if stepsFlag == "" {
		return fmt.Sprintf("Steps: NOT REPORTED (?/%d)", len(allStepIDs)), nil
	}

	reported, err := parseStepResults(stepsFlag)
	if err != nil {
		return "", err
	}

	// Warn about entries that name no canonical step; they never reach the
	// audit line, which could otherwise hide a typo in the statuses.
	canonical := make(map[string]bool, len(allStepIDs))
	for _, id := range allStepIDs {
		canonical[id] = true
	}
	for id := range reported {
		if !canonical[id] {
			fmt.Fprintf(out, "warning: step %q not in %s — ignored\n", id, formulaName)
		}
	}

	// Build the audit line: map each formula step to its reported status. A
	// step missing from the report was skipped — SKIP is the one default the
	// audit keeps, because an agent that actually ran a step and omitted it
	// is claiming less than it did. The failure mode to prevent is the
	// inverse: entries that parsed without a status (bare ids on the CLI).
	var parts []string
	okCount := 0
	for _, stepID := range allStepIDs {
		status, ok := reported[stepID]
		if !ok {
			status = "SKIP"
		}
		if status == "OK" {
			okCount++
		}
		parts = append(parts, stepID+" "+status)
	}

	return fmt.Sprintf("Steps: %s (%d/%d)", strings.Join(parts, " | "), okCount, len(allStepIDs)), nil
}

// parseStepResults parses a comma-separated string of step:STATUS pairs and
// returns a map of step ID to uppercase status.
// Example input: "heartbeat:OK,inbox-check:OK,orphan-cleanup:SKIP"
//
// A bare id (no ":STATUS") is always a parse error: the audit is the
// anti-shortcut record of the ledger, so silently defaulting a status-less
// entry to SKIP is what turned a full 28/28 deacon patrol into a ledger
// entry reading 0/28 (gt-gvo8m). Every caller is the CLI path; the refinery
// unit cycle reports no steps at all.
func parseStepResults(stepsFlag string) (map[string]string, error) {
	results := make(map[string]string)
	for _, entry := range strings.Split(stepsFlag, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("entry %q has no status — use %q (expected step:STATUS pairs like heartbeat:OK,inbox-check:OK)", entry, entry+":OK")
		}
		results[strings.TrimSpace(parts[0])] = strings.ToUpper(strings.TrimSpace(parts[1]))
	}
	return results, nil
}
