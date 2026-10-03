package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/schedulerrun"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	schedulerStatusJSON bool
	schedulerListJSON   bool
	schedulerClearBead  string
	schedulerRunBatch   int
	schedulerRunDryRun  bool
)

var schedulerCmd = &cobra.Command{
	Use:     "scheduler",
	GroupID: GroupWork,
	Short:   "Manage dispatch scheduler",
	Long: `Manage the capacity-controlled dispatch scheduler.

Subcommands:
  gt scheduler status    # Show scheduler state
  gt scheduler list      # List all scheduled beads
  gt scheduler run       # Manual dispatch trigger
  gt scheduler pause     # Pause dispatch
  gt scheduler resume    # Resume dispatch
  gt scheduler clear     # Remove beads from scheduler

Config:
  gt config set scheduler.max_polecats 5    # Enable deferred dispatch
  gt config set scheduler.max_polecats -1   # Direct dispatch (default)`,
	RunE: requireSubcommand,
}

var schedulerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show scheduler state: pending, capacity, active polecats",
	RunE:  runSchedulerStatus,
}

var schedulerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all scheduled beads with titles, rig, blocked status",
	RunE:  runSchedulerList,
}

var schedulerPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Pause all scheduler dispatch (town-wide)",
	RunE:  runSchedulerPause,
}

var schedulerResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume scheduler dispatch",
	RunE:  runSchedulerResume,
}

var schedulerClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Remove beads from the scheduler",
	Long: `Remove beads from the scheduler by closing sling context beads.

Without --bead, removes ALL beads from the scheduler.
With --bead, removes only the specified bead.`,
	RunE: runSchedulerClear,
}

var schedulerRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Manually trigger scheduler dispatch",
	Long: `Manually trigger dispatch of scheduled work.

This dispatches scheduled beads using the same logic as the daemon heartbeat,
but can be run ad-hoc. Useful for testing or when the daemon is not running.

  gt scheduler run                  # Dispatch using config defaults
  gt scheduler run --batch 5        # Dispatch up to 5
  gt scheduler run --dry-run        # Preview what would dispatch`,
	RunE: runSchedulerRun,
}

func init() {
	// Status flags
	schedulerStatusCmd.Flags().BoolVar(&schedulerStatusJSON, "json", false, "Output as JSON")

	// List flags
	schedulerListCmd.Flags().BoolVar(&schedulerListJSON, "json", false, "Output as JSON")

	// Clear flags
	schedulerClearCmd.Flags().StringVar(&schedulerClearBead, "bead", "", "Remove specific bead from scheduler")

	// Run flags
	schedulerRunCmd.Flags().IntVar(&schedulerRunBatch, "batch", 0, "Override batch size (0 = use config)")
	schedulerRunCmd.Flags().BoolVar(&schedulerRunDryRun, "dry-run", false, "Preview what would dispatch")

	// Build command tree (flat — no intermediary "capacity" level)
	schedulerCmd.AddCommand(schedulerStatusCmd)
	schedulerCmd.AddCommand(schedulerListCmd)
	schedulerCmd.AddCommand(schedulerPauseCmd)
	schedulerCmd.AddCommand(schedulerResumeCmd)
	schedulerCmd.AddCommand(schedulerClearCmd)
	schedulerCmd.AddCommand(schedulerRunCmd)

	rootCmd.AddCommand(schedulerCmd)
}

func runSchedulerStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	state, err := capacity.LoadState(townRoot)
	if err != nil {
		return fmt.Errorf("loading scheduler state: %w", err)
	}

	scheduled, err := schedulerrun.ListScheduled(townRoot)
	if err != nil {
		return fmt.Errorf("listing scheduled beads: %w", err)
	}

	capacitySnapshot, err := polecatCapacitySnapshotForTown(townRoot)
	if err != nil {
		return fmt.Errorf("loading polecat capacity: %w", err)
	}

	if schedulerStatusJSON {
		out := struct {
			Paused         bool                       `json:"paused"`
			PausedBy       string                     `json:"paused_by,omitempty"`
			ScheduledTotal int                        `json:"queued_total"`
			ScheduledReady int                        `json:"queued_ready"`
			ActivePolecats int                        `json:"active_polecats"`
			Capacity       polecatCapacitySnapshot    `json:"capacity"`
			LastDispatchAt string                     `json:"last_dispatch_at,omitempty"`
			Beads          []schedulerrun.ContextInfo `json:"beads"`
		}{
			Paused:         state.Paused,
			PausedBy:       state.PausedBy,
			ScheduledTotal: len(scheduled),
			ActivePolecats: capacitySnapshot.ActiveSessions,
			Capacity:       capacitySnapshot,
			LastDispatchAt: state.LastDispatchAt,
			Beads:          scheduled,
		}
		for _, b := range scheduled {
			if !b.Blocked {
				out.ScheduledReady++
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	readyCount := 0
	for _, b := range scheduled {
		if !b.Blocked {
			readyCount++
		}
	}

	fmt.Printf("%s\n\n", style.Bold.Render("Scheduler Status"))
	if state.Paused {
		fmt.Printf("  State:    %s (by %s)\n", style.Warning.Render("PAUSED"), state.PausedBy)
	} else {
		fmt.Printf("  State:    active\n")
	}
	fmt.Printf("  Scheduled: %d total, %d ready\n", len(scheduled), readyCount)
	fmt.Printf("  Active:    %d polecats\n", capacitySnapshot.ActiveSessions)
	if capacitySnapshot.Max > 0 {
		fmt.Printf("  Capacity:  %d free of %d (working: %d, recovery: %d, reservations: %d, reusable idle: %d, parked: %d, pending MR: %d)\n",
			capacitySnapshot.Free,
			capacitySnapshot.Max,
			capacitySnapshot.Working,
			capacitySnapshot.RecoveryBlocked,
			capacitySnapshot.Reservations,
			capacitySnapshot.ReusableIdle,
			capacitySnapshot.Parked,
			capacitySnapshot.PendingMR,
		)
	} else {
		fmt.Printf("  Capacity:  direct dispatch (scheduler.max_polecats=%d)\n", capacitySnapshot.Max)
	}
	if state.LastDispatchAt != "" {
		fmt.Printf("  Last dispatch: %s (%d beads)\n", state.LastDispatchAt, state.LastDispatchCount)
	}

	return nil
}

func runSchedulerList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	scheduled, err := schedulerrun.ListScheduled(townRoot)
	if err != nil {
		return fmt.Errorf("listing scheduled beads: %w", err)
	}

	if schedulerListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(scheduled)
	}

	if len(scheduled) == 0 {
		fmt.Println("No beads scheduled.")
		fmt.Println("Enable deferred dispatch with: gt config set scheduler.max_polecats <N>")
		return nil
	}

	byRig := make(map[string][]schedulerrun.ContextInfo)
	for _, b := range scheduled {
		byRig[b.TargetRig] = append(byRig[b.TargetRig], b)
	}

	fmt.Printf("%s (%d beads)\n\n", style.Bold.Render("Scheduled Work"), len(scheduled))
	for rig, beads := range byRig {
		fmt.Printf("  %s (%d):\n", style.Bold.Render(rig), len(beads))
		for _, b := range beads {
			indicator := "○"
			if b.Blocked {
				indicator = "⏸"
			}
			detail := ""
			if b.MergePending {
				// Distinguish "waiting on unlanded work" from ordinary
				// blocking: a blocked-on-queue bead is unblocked by a merge,
				// not by anything the scheduler does.
				detail = " (waiting on merge"
				if b.BlockerMR != "" {
					detail += " " + b.BlockerMR
				}
				detail += ")"
			}
			fmt.Printf("    %s %s: %s%s\n", indicator, b.ID, b.Title, detail)
		}
		fmt.Println()
	}

	return nil
}

func runSchedulerPause(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	state, err := capacity.LoadState(townRoot)
	if err != nil {
		return fmt.Errorf("loading scheduler state: %w", err)
	}

	if state.Paused {
		fmt.Printf("%s Scheduler is already paused (by %s)\n", style.Dim.Render("○"), state.PausedBy)
		return nil
	}

	actor := detectActor()
	state.SetPaused(actor)
	if err := capacity.SaveState(townRoot, state); err != nil {
		return fmt.Errorf("saving scheduler state: %w", err)
	}

	fmt.Printf("%s Scheduler paused\n", style.Bold.Render("⏸"))
	return nil
}

func runSchedulerResume(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	state, err := capacity.LoadState(townRoot)
	if err != nil {
		return fmt.Errorf("loading scheduler state: %w", err)
	}

	if !state.Paused {
		fmt.Printf("%s Scheduler is not paused\n", style.Dim.Render("○"))
		return nil
	}

	state.SetResumed()
	if err := capacity.SaveState(townRoot, state); err != nil {
		return fmt.Errorf("saving scheduler state: %w", err)
	}

	fmt.Printf("%s Scheduler resumed\n", style.Bold.Render("▶"))
	return nil
}

// beadsForContextRecord opens the beads database one sling context lives in.
// Contexts are created in the target rig's beads dir (GH#3468), which
// ListOpenSlingContextRecords reports per record.
func beadsForContextRecord(rec beads.SlingContextRecord) *beads.Beads {
	return beads.NewWithBeadsDir(rec.WorkDir, rec.BeadsDir)
}

func runSchedulerClear(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	if schedulerClearBead != "" {
		// Close ALL sling contexts for this specific work bead (there may be
		// duplicates if concurrent scheduleBead calls raced past idempotency).
		// Scan all rig dirs since contexts live in target rig beads. (GH#3468)
		contexts, err := beads.ListOpenSlingContextRecords(townRoot)
		if err != nil {
			return fmt.Errorf("listing sling contexts: %w", err)
		}

		closed := 0
		for _, ctx := range contexts {
			fields := beads.ParseSlingContextFields(ctx.Issue.Description)
			if fields != nil && fields.WorkBeadID == schedulerClearBead {
				if err := beads.CloseSlingContext(beadsForContextRecord(ctx), ctx.Issue.ID, "cleared"); err != nil {
					fmt.Printf("  %s Could not close context %s: %v\n", style.Dim.Render("Warning:"), ctx.Issue.ID, err)
					continue
				}
				closed++
			}
		}

		if closed == 0 {
			fmt.Printf("%s No sling context found for %s\n", style.Dim.Render("○"), schedulerClearBead)
		} else {
			fmt.Printf("%s Removed %s from scheduler (closed %d context(s))\n",
				style.Bold.Render("✓"), schedulerClearBead, closed)
		}
		return nil
	}

	// Close all open sling contexts across all dirs
	allContexts, err := beads.ListOpenSlingContextRecords(townRoot)
	if err != nil {
		return fmt.Errorf("listing sling contexts: %w", err)
	}

	if len(allContexts) == 0 {
		fmt.Println("Scheduler is already empty.")
		return nil
	}

	cleared := 0
	for _, ctx := range allContexts {
		if err := beads.CloseSlingContext(beadsForContextRecord(ctx), ctx.Issue.ID, "cleared"); err != nil {
			fmt.Printf("  %s Could not close context %s: %v\n", style.Dim.Render("Warning:"), ctx.Issue.ID, err)
			continue
		}
		cleared++
	}

	fmt.Printf("%s Cleared %d context bead(s) from scheduler\n", style.Bold.Render("✓"), cleared)
	return nil
}

func runSchedulerRun(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	report, err := schedulerrun.Run(cmd.Context(), schedulerrun.Options{
		TownRoot:      townRoot,
		Actor:         detectActor(),
		BatchOverride: schedulerRunBatch,
		DryRun:        schedulerRunDryRun,
	}, schedulerDeps())
	if err != nil {
		return err
	}

	// Nothing needs waking: the daemon's patrol_scan tick watches the new
	// polecat (gt-4k3fj.6). Warn when the daemon is not running, since then
	// nothing restarts a polecat whose session dies (gt-9wv0).
	for _, rig := range report.Rigs {
		wakeRigAgents(rig)
	}
	return nil
}

// countActivePolecats counts all running polecat tmux sessions across all rigs.
// Capacity admission uses polecatCapacitySnapshotForTown instead; active sessions
// are shown for operator context only.
func countActivePolecats(townRoot string) int {
	reg := townRegistry()
	names, err := listTownTmuxSessions(townRoot)
	if err != nil {
		return 0
	}

	count := 0
	for _, line := range names {
		identity, err := session.ParseSessionNameWithRegistry(line, reg)
		if err != nil {
			continue
		}
		if identity.Role == session.RolePolecat {
			count++
		}
	}
	return count
}
