package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// defaultOrphanGraceSecs is the grace period (in seconds) between SIGTERM and SIGKILL
// when automatically cleaning up orphaned Claude processes during shutdown.
// This is shorter than the --cleanup-orphans-grace-secs default (60s) because
// automatic cleanup runs after sessions are already killed, so processes have
// already had time to shut down.
const defaultOrphanGraceSecs = 5

var (
	shutdownGraceful            bool
	shutdownWait                int
	shutdownAll                 bool
	shutdownForce               bool
	shutdownYes                 bool
	shutdownPolecatsOnly        bool
	shutdownNuclear             bool
	shutdownCleanupOrphans      bool
	shutdownCleanupOrphansGrace int
)

var shutdownCmd = &cobra.Command{
	Use:     "shutdown",
	GroupID: GroupServices,
	Short:   "Shutdown Gas Town with cleanup",
	Long: `Shutdown Gas Town by stopping agents and cleaning up polecats.

This is the "done for the day" command - it stops everything AND removes
polecat worktrees/branches. For a reversible pause, use 'gt down' instead.

Comparison:
  gt down      - Pause (stop processes, keep worktrees) - reversible
  gt shutdown  - Done (stop + cleanup worktrees) - permanent cleanup

After killing sessions, polecats are cleaned up:
  - Worktrees are removed
  - Polecat branches are deleted
  - Polecats with uncommitted work are SKIPPED (protected)

Shutdown levels (progressively more aggressive):
  (default)       - Stop infrastructure + polecats + cleanup
  --all           - Also stop crew sessions
  --polecats-only - Only stop polecats (leaves infrastructure running)

Use --force or --yes to skip confirmation prompt.
Use --graceful to allow agents time to save state before killing.
Use --nuclear to force cleanup even if polecats have uncommitted work (DANGER).
Use --cleanup-orphans to use a longer grace period for orphan cleanup (default 60s).
Use --cleanup-orphans-grace-secs to set that grace period.

Orphaned Claude processes are always cleaned up after session termination.
By default, a 5-second grace period is used. The --cleanup-orphans flag
extends this to --cleanup-orphans-grace-secs (default 60s) for stubborn processes.`,
	Args: cobra.NoArgs,
	RunE: runShutdown,
}

func init() {
	shutdownCmd.Flags().BoolVarP(&shutdownGraceful, "graceful", "g", false,
		"Send ESC to agents and wait for them to handoff before killing")
	shutdownCmd.Flags().IntVarP(&shutdownWait, "wait", "w", 30,
		"Seconds to wait for graceful shutdown (default 30)")
	shutdownCmd.Flags().BoolVarP(&shutdownAll, "all", "a", false,
		"Also stop crew sessions (by default, crew is preserved)")
	shutdownCmd.Flags().BoolVarP(&shutdownForce, "force", "f", false,
		"Skip confirmation prompt (alias for --yes)")
	shutdownCmd.Flags().BoolVarP(&shutdownYes, "yes", "y", false,
		"Skip confirmation prompt")
	shutdownCmd.Flags().BoolVar(&shutdownPolecatsOnly, "polecats-only", false,
		"Only stop polecats (minimal shutdown)")
	shutdownCmd.Flags().BoolVar(&shutdownNuclear, "nuclear", false,
		"Force cleanup even if polecats have uncommitted work (DANGER: may lose work)")
	shutdownCmd.Flags().BoolVar(&shutdownCleanupOrphans, "cleanup-orphans", false,
		"Use longer grace period (--cleanup-orphans-grace-secs) for orphan cleanup instead of default 5s")
	shutdownCmd.Flags().IntVar(&shutdownCleanupOrphansGrace, "cleanup-orphans-grace-secs", 60,
		"Grace period in seconds between SIGTERM and SIGKILL when cleaning orphans (default 60)")

	rootCmd.AddCommand(shutdownCmd)
}

func runShutdown(cmd *cobra.Command, args []string) error {
	t := tmux.NewTmux()

	// Find workspace root for polecat cleanup
	townRoot, _ := workspace.FindFromCwd()

	// Collect sessions to show what will be stopped
	sessions, err := t.ListSessions()
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	toStop, preserved := categorizeSessions(townRegistry(), sessions)

	if len(toStop) == 0 {
		fmt.Printf("%s Gas Town was not running\n", style.Dim.Render("○"))

		// Still check for orphaned daemons even if no sessions are running
		if townRoot != "" {
			fmt.Println()
			fmt.Println("Checking for orphaned daemon...")
			stopDaemonIfRunning(townRoot)
		}

		return nil
	}

	// Show what will happen
	fmt.Println("Sessions to stop:")
	for _, sess := range toStop {
		fmt.Printf("  %s %s\n", style.Bold.Render("→"), sess)
	}
	if len(preserved) > 0 && !shutdownAll {
		fmt.Println()
		fmt.Println("Sessions preserved (crew):")
		for _, sess := range preserved {
			fmt.Printf("  %s %s\n", style.Dim.Render("○"), sess)
		}
	}
	fmt.Println()

	// Confirmation prompt
	if !shutdownYes && !shutdownForce {
		fmt.Printf("Proceed with shutdown? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Shutdown canceled.")
			return nil
		}
	}

	if shutdownGraceful {
		return runGracefulShutdown(t, toStop, townRoot)
	}
	return runImmediateShutdown(t, toStop, townRoot)
}

// categorizeSessions splits sessions into those to stop and those to preserve.
func categorizeSessions(reg *session.PrefixRegistry, sessions []string) (toStop, preserved []string) {
	for _, sess := range sessions {
		// Gas Town sessions use rig-specific prefixes or hq- (town-level)
		if !reg.IsKnownSession(sess) {
			continue // Not a Gas Town session
		}

		// Parse session to determine role
		isPolecat := false
		isCrew := false
		if identity, err := session.ParseSessionNameWithRegistry(sess, reg); err == nil {
			switch identity.Role {
			case session.RolePolecat:
				isPolecat = true
			case session.RoleCrew:
				isCrew = true
			}
		}

		// Decide based on flags
		if shutdownPolecatsOnly {
			// Only stop polecats
			if isPolecat {
				toStop = append(toStop, sess)
			} else {
				preserved = append(preserved, sess)
			}
		} else if shutdownAll {
			// Stop everything including crew
			toStop = append(toStop, sess)
		} else {
			// Default: preserve crew
			if isCrew {
				preserved = append(preserved, sess)
			} else {
				toStop = append(toStop, sess)
			}
		}
	}
	return
}

func runGracefulShutdown(t *tmux.Tmux, gtSessions []string, townRoot string) error {
	fmt.Printf("Graceful shutdown of Gas Town (waiting up to %ds)...\n\n", shutdownWait)

	// Phase 1: Send ESC to all agents to interrupt them
	fmt.Printf("Phase 1: Sending ESC to %d agent(s)...\n", len(gtSessions))
	for _, sess := range gtSessions {
		fmt.Printf("  %s Interrupting %s\n", style.Bold.Render("→"), sess)
		_ = t.SendKeysRaw(sess, "Escape") // best-effort interrupt
	}

	// Phase 2: Send shutdown message asking agents to handoff
	fmt.Printf("\nPhase 2: Requesting handoff from agents...\n")
	shutdownMsg := "[SHUTDOWN] Gas Town is shutting down. Please save your state and update your handoff bead, then type /exit or wait to be terminated."
	for _, sess := range gtSessions {
		// Small delay then send the message
		time.Sleep(constants.ShutdownNotifyDelay)
		_ = t.SendKeys(sess, shutdownMsg) // best-effort notification
	}

	// Phase 3: Wait for agents to complete handoff
	fmt.Printf("\nPhase 3: Waiting %ds for agents to complete handoff...\n", shutdownWait)
	fmt.Printf("  %s\n", style.Dim.Render("(Press Ctrl-C to force immediate shutdown)"))

	// Wait with countdown
	for remaining := shutdownWait; remaining > 0; remaining -= 5 {
		if remaining < shutdownWait {
			fmt.Printf("  %s %ds remaining...\n", style.Dim.Render("⏳"), remaining)
		}
		sleepTime := 5
		if remaining < 5 {
			sleepTime = remaining
		}
		time.Sleep(time.Duration(sleepTime) * time.Second)
	}

	// Phase 4: Kill sessions in correct order
	fmt.Printf("\nPhase 4: Terminating sessions...\n")
	mayorSession := getMayorSessionName()
	stopped := killSessionsInOrder(t, gtSessions, mayorSession)

	// Phase 5: Always clean up orphaned Claude processes after killing sessions.
	// Processes can survive session kills if they caught/ignored SIGHUP or called setsid().
	// Use the user-specified grace period if --cleanup-orphans was explicitly set,
	// otherwise use a short default (5s) for the automatic sweep.
	graceSecs := defaultOrphanGraceSecs
	if shutdownCleanupOrphans {
		graceSecs = shutdownCleanupOrphansGrace
	}
	fmt.Printf("\nPhase 5: Cleaning up orphaned Claude processes...\n")
	cleanupOrphanedClaude(graceSecs)

	// Phase 6: Cleanup polecat worktrees and branches
	fmt.Printf("\nPhase 6: Cleaning up polecats...\n")
	if townRoot != "" {
		cleanupPolecats(townRoot)
	}

	// Phase 7: Stop the daemon
	fmt.Printf("\nPhase 7: Stopping daemon...\n")
	if townRoot != "" {
		stopDaemonIfRunning(townRoot)
	}

	// Phase 8: Verify no Claude processes survived
	fmt.Printf("\nPhase 8: Verifying shutdown...\n")
	verifyNoOrphans()

	fmt.Println()
	fmt.Printf("%s Graceful shutdown complete (%d sessions stopped)\n", style.Bold.Render("✓"), stopped)
	return nil
}

func runImmediateShutdown(t *tmux.Tmux, gtSessions []string, townRoot string) error {
	fmt.Println("Shutting down Gas Town...")

	mayorSession := getMayorSessionName()
	stopped := killSessionsInOrder(t, gtSessions, mayorSession)

	// Always clean up orphaned Claude processes after killing sessions.
	// Processes can survive session kills if they caught/ignored SIGHUP or called setsid().
	// Use the user-specified grace period if --cleanup-orphans was explicitly set,
	// otherwise use a short default (5s) for the automatic sweep.
	graceSecs := defaultOrphanGraceSecs
	if shutdownCleanupOrphans {
		graceSecs = shutdownCleanupOrphansGrace
	}
	fmt.Println()
	fmt.Println("Cleaning up orphaned Claude processes...")
	cleanupOrphanedClaude(graceSecs)

	// Cleanup polecat worktrees and branches
	if townRoot != "" {
		fmt.Println()
		fmt.Println("Cleaning up polecats...")
		cleanupPolecats(townRoot)
	}

	// Stop the daemon
	if townRoot != "" {
		fmt.Println()
		fmt.Println("Stopping daemon...")
		stopDaemonIfRunning(townRoot)
	}

	// Verify no Claude processes survived
	fmt.Println()
	fmt.Println("Verifying shutdown...")
	verifyNoOrphans()

	fmt.Println()
	fmt.Printf("%s Gas Town shutdown complete (%d sessions stopped)\n", style.Bold.Render("✓"), stopped)

	return nil
}

// killSessionsInOrder stops sessions in the correct shutdown order, matching gt down:
// every rig-level session (polecats, crew, and any session left over from a
// retired role) first, then the Mayor. mayorSession is the dynamic Mayor
// session name for the current town.
//
// Returns the count of sessions that were successfully stopped (verified by checking
// if the session no longer exists after the kill attempt).
func killSessionsInOrder(t *tmux.Tmux, sessions []string, mayorSession string) int {
	stopped := 0

	// Helper to kill a session and verify it was stopped
	killAndVerify := func(sess string) bool {
		// Check if session exists before attempting to kill
		exists, _ := t.HasSession(sess)
		if !exists {
			return false // Session already gone
		}

		// Attempt to kill the session and its processes
		_ = t.KillSessionWithProcesses(sess)

		// Verify the session is actually gone (ignore error, check existence)
		// KillSessionWithProcesses might return an error even if it successfully
		// killed the processes and the session auto-closed
		stillExists, _ := t.HasSession(sess)
		if !stillExists {
			fmt.Printf("  %s %s stopped\n", style.Bold.Render("✓"), sess)
			return true
		}
		return false
	}

	hasMayor := false
	for _, sess := range sessions {
		if sess == mayorSession {
			hasMayor = true
			continue
		}
		if killAndVerify(sess) {
			stopped++
		}
	}
	if hasMayor && killAndVerify(mayorSession) {
		stopped++
	}

	return stopped
}

// cleanupPolecats removes polecat worktrees and branches for all rigs.
// It refuses to clean up polecats with uncommitted work unless --nuclear is set.
func cleanupPolecats(townRoot string) {
	// Load rigs config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		fmt.Printf("  %s Could not load rigs config: %v\n", style.Dim.Render("○"), err)
		return
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)

	// Discover all rigs
	rigs, err := rigMgr.DiscoverRigs()
	if err != nil {
		fmt.Printf("  %s Could not discover rigs: %v\n", style.Dim.Render("○"), err)
		return
	}

	totalCleaned := 0
	totalSkipped := 0
	var uncommittedPolecats []string

	for _, r := range rigs {
		polecatGit := git.NewGit(r.Path)
		polecatMgr := polecat.NewManager(r, polecatGit, nil) // nil tmux: just listing, not allocating

		polecats, err := polecatMgr.List()
		if err != nil {
			continue
		}

		for _, p := range polecats {
			// Check for uncommitted work
			pGit := git.NewGit(p.ClonePath)
			status, err := pGit.CheckUncommittedWork()
			if err != nil {
				// Can't check, be safe and skip unless nuclear
				if !shutdownNuclear {
					fmt.Printf("  %s %s/%s: could not check status, skipping\n",
						style.Dim.Render("○"), r.Name, p.Name)
					totalSkipped++
					continue
				}
			} else if !status.Clean() {
				// Has uncommitted work
				if !shutdownNuclear {
					uncommittedPolecats = append(uncommittedPolecats,
						fmt.Sprintf("%s/%s (%s)", r.Name, p.Name, status.String()))
					totalSkipped++
					continue
				}
				// Nuclear mode: warn but proceed
				fmt.Printf("  %s %s/%s: NUCLEAR - removing despite %s\n",
					style.Bold.Render("⚠"), r.Name, p.Name, status.String())
			}

			// Clean: remove worktree and branch.
			// SelfNuke stays false: this is gt shutdown cleanup, not polecat self-deleting.
			if err := polecatMgr.RemoveWithOptions(p.Name, polecat.RemoveOptions{Force: true, Nuclear: shutdownNuclear}); err != nil {
				fmt.Printf("  %s %s/%s: cleanup failed: %v\n",
					style.Dim.Render("○"), r.Name, p.Name, err)
				totalSkipped++
				continue
			}

			// Delete the polecat branch from mayor's clone
			branchName := fmt.Sprintf("polecat/%s", p.Name)
			mayorPath := filepath.Join(r.Path, "mayor", "rig")
			mayorGit := git.NewGit(mayorPath)
			_ = mayorGit.DeleteBranch(branchName, true) // Ignore errors

			fmt.Printf("  %s %s/%s: cleaned up\n", style.Bold.Render("✓"), r.Name, p.Name)
			totalCleaned++
		}
	}

	// Summary
	if len(uncommittedPolecats) > 0 {
		fmt.Println()
		fmt.Printf("  %s Polecats with uncommitted work (use --nuclear to force):\n",
			style.Bold.Render("⚠"))
		for _, pc := range uncommittedPolecats {
			fmt.Printf("    • %s\n", pc)
		}
	}

	if totalCleaned > 0 || totalSkipped > 0 {
		fmt.Printf("  Cleaned: %d, Skipped: %d\n", totalCleaned, totalSkipped)
	} else {
		fmt.Printf("  %s No polecats to clean up\n", style.Dim.Render("○"))
	}
}

// stopDaemonIfRunning stops the daemon if it is running.
// This prevents the daemon from restarting agents after shutdown.
// Uses robust detection with fallback to process search.
func stopDaemonIfRunning(townRoot string) {
	// Primary detection: PID file
	running, pid, err := daemon.IsRunning(townRoot)

	if err != nil {
		// Detection error - report it but continue with fallback
		fmt.Printf("  %s Daemon detection warning: %s\n", style.Bold.Render("⚠"), err.Error())
	}

	if running {
		// PID file points to live daemon - stop it
		if err := daemon.StopDaemon(townRoot); err != nil {
			fmt.Printf("  %s Failed to stop daemon (PID %d): %s\n",
				style.Bold.Render("✗"), pid, err.Error())
		} else {
			fmt.Printf("  %s Daemon stopped (was PID %d)\n", style.Bold.Render("✓"), pid)
		}
	} else {
		fmt.Printf("  %s Daemon not tracked by PID file\n", style.Dim.Render("○"))
	}

	// Fallback: Search for orphaned daemon processes
	orphaned, err := daemon.FindOrphanedDaemons(townRoot)
	if err != nil {
		fmt.Printf("  %s Warning: failed to search for orphaned daemons: %v\n",
			style.Dim.Render("○"), err)
		return
	}

	if len(orphaned) > 0 {
		fmt.Printf("  %s Found %d orphaned daemon process(es): %v\n",
			style.Bold.Render("⚠"), len(orphaned), orphaned)

		killed, err := daemon.KillOrphanedDaemons(townRoot)
		if err != nil {
			fmt.Printf("  %s Failed to kill orphaned daemons: %v\n",
				style.Bold.Render("✗"), err)
		} else if killed > 0 {
			fmt.Printf("  %s Killed %d orphaned daemon(s)\n",
				style.Bold.Render("✓"), killed)
		}
	}
}
