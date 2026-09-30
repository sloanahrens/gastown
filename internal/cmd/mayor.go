package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var mayorCmd = &cobra.Command{
	Use:     "mayor",
	Aliases: []string{"may"},
	GroupID: GroupAgents,
	Short:   "Manage the Mayor (Chief of Staff for cross-rig coordination)",
	RunE:    requireSubcommand,
	Long: `Manage the Mayor - the Overseer's Chief of Staff.

The Mayor is the global coordinator for Gas Town:
  - Receives escalations from Witnesses and Deacon
  - Coordinates work across multiple rigs
  - Handles human communication when needed
  - Routes strategic decisions and cross-project issues

The Mayor is the primary interface between the human Overseer and the
automated agents. When in doubt, escalate to the Mayor.

Role shortcuts: "mayor" in mail/nudge addresses resolves to this agent.`,
}

var (
	mayorAgentOverride string
	mayorStatusRunning bool
)

var mayorStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Mayor session",
	Long: `Start the Mayor tmux session.

Creates a new detached tmux session for the Mayor and launches Claude.
The session runs in the workspace root directory.`,
	RunE: runMayorStart,
}

var mayorStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Mayor session",
	Long: `Stop the Mayor tmux session.

Attempts graceful shutdown first (Ctrl-C), then kills the tmux session.`,
	RunE: runMayorStop,
}

var mayorAttachCmd = &cobra.Command{
	Use:     "attach",
	Aliases: []string{"at"},
	Short:   "Attach to the Mayor session",
	Long: `Attach to the running Mayor tmux session.

Attaches the current terminal to the Mayor's tmux session.
Detach with Ctrl-B D.`,
	RunE: runMayorAttach,
}

var mayorStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check Mayor session status",
	Long:  `Check if the Mayor tmux session is currently running.`,
	RunE:  runMayorStatus,
}

var mayorRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Mayor session",
	Long: `Restart the Mayor tmux session.

Stops the current session (if running) and starts a fresh one.`,
	RunE: runMayorRestart,
}

func init() {
	mayorCmd.AddCommand(mayorStartCmd)
	mayorCmd.AddCommand(mayorStopCmd)
	mayorCmd.AddCommand(mayorAttachCmd)
	mayorCmd.AddCommand(mayorStatusCmd)
	mayorCmd.AddCommand(mayorRestartCmd)

	mayorStatusCmd.Flags().BoolVar(&mayorStatusRunning, "running", false, "Output only true/false for running status")

	mayorStartCmd.Flags().StringVar(&mayorAgentOverride, "agent", "", "Agent alias to run the Mayor with (overrides town default)")
	mayorAttachCmd.Flags().StringVar(&mayorAgentOverride, "agent", "", "Agent alias to run the Mayor with (overrides town default)")
	mayorRestartCmd.Flags().StringVar(&mayorAgentOverride, "agent", "", "Agent alias to run the Mayor with (overrides town default)")

	rootCmd.AddCommand(mayorCmd)
}

// getMayorManager returns a mayor manager for the current workspace.
func getMayorManager() (*mayor.Manager, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return mayor.NewManager(townRoot), nil
}

// getMayorSessionName returns the Mayor session name.
func getMayorSessionName() string {
	return mayor.SessionName()
}

func runMayorStart(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	fmt.Println("Starting Mayor session...")
	if err := mgr.Start(mayorAgentOverride); err != nil {
		if err == mayor.ErrAlreadyRunning {
			return fmt.Errorf("Mayor session already running. Attach with: gt mayor attach")
		}
		return err
	}

	fmt.Printf("%s Mayor session started. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render("gt mayor attach"))

	return nil
}

func runMayorStop(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	fmt.Println("Stopping Mayor session...")
	if err := mgr.Stop(); err != nil {
		if err == mayor.ErrNotRunning {
			return fmt.Errorf("Mayor session is not running")
		}
		return err
	}

	fmt.Printf("%s Mayor session stopped.\n", style.Bold.Render("✓"))
	return nil
}

func runMayorAttach(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}

	// Ensure daemon and dolt are running before attaching.
	if err := ensureMayorInfra(townRoot); err != nil {
		return err
	}

	t := tmux.NewTmux()
	sessionID := mgr.SessionName()

	running, err := mgr.IsRunning()
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		// Auto-start if not running
		fmt.Println("Mayor session not running, starting...")
		if err := mgr.Start(mayorAgentOverride); err != nil {
			return err
		}
	} else {
		if err := restartMayorRuntimeIfDead(t, sessionID, townRoot); err != nil {
			return err
		}
	}

	// Use shared attach helper (smart: links if inside tmux, attaches if outside)
	return attachToTmuxSession(sessionID)
}

// mayorRuntimeTmux is the tmux surface restartMayorRuntimeIfDead drives.
type mayorRuntimeTmux interface {
	IsAgentAliveChecked(session string) (bool, error)
	GetPaneID(session string) (string, error)
	SetEnvironment(session, key, value string) error
	SetRemainOnExit(pane string, on bool) error
	KillPaneProcesses(pane string) error
	RespawnPane(pane, command string) error
}

var _ mayorRuntimeTmux = (*tmux.Tmux)(nil)

// restartMayorRuntimeIfDead respawns the Mayor's runtime inside an existing
// session when the agent has exited (hq-95xfq, gt-7zl). It uses
// IsAgentAliveChecked (descendant processes) rather than the pane command,
// since the Mayor launches via a bash wrapper. A failed liveness query is
// unknown: it warns and leaves the session alone (gt-fcxe9.1).
func restartMayorRuntimeIfDead(t mayorRuntimeTmux, sessionID, townRoot string) error {
	alive, aliveErr := t.IsAgentAliveChecked(sessionID)
	if aliveErr != nil {
		style.PrintWarning("could not verify the Mayor agent is running (%v); attaching without restart", aliveErr)
		return nil
	}
	if alive {
		return nil
	}

	// Runtime has exited, restart it with proper context
	fmt.Println("Runtime exited, restarting with context...")

	paneID, err := t.GetPaneID(sessionID)
	if err != nil {
		return fmt.Errorf("getting pane ID: %w", err)
	}

	// Build startup beacon for context (like gt handoff does)
	beacon := session.FormatStartupBeacon(session.BeaconConfig{
		Recipient: "mayor",
		Sender:    "human",
		Topic:     "attach",
	})

	// Build startup command with beacon
	startupCmd, err := config.BuildAgentStartupCommandWithAgentOverride("mayor", "", townRoot, "", beacon, mayorAgentOverride)
	if err != nil {
		return fmt.Errorf("building startup command: %w", err)
	}

	// Resolve CLAUDE_CONFIG_DIR and prepend it so the respawned process
	// uses the correct account (mirrors what StartTMUX does).
	accountsPath := constants.MayorAccountsPath(townRoot)
	claudeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if claudeConfigDir == "" {
		claudeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if claudeConfigDir != "" {
		startupCmd = config.PrependEnv(startupCmd, map[string]string{"CLAUDE_CONFIG_DIR": claudeConfigDir})
		_ = t.SetEnvironment(sessionID, "CLAUDE_CONFIG_DIR", claudeConfigDir)
	}

	// Set remain-on-exit so the pane survives process death during respawn.
	// Without this, killing processes causes tmux to destroy the pane.
	if err := t.SetRemainOnExit(paneID, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}

	// Kill all processes in the pane before respawning to prevent orphan leaks
	// RespawnPane's -k flag only sends SIGHUP which Claude/Node may ignore
	if err := t.KillPaneProcesses(paneID); err != nil {
		// Non-fatal but log the warning
		style.PrintWarning("could not kill pane processes: %v", err)
	}

	// Note: respawn-pane automatically resets remain-on-exit to off
	if err := t.RespawnPane(paneID, startupCmd); err != nil {
		return fmt.Errorf("restarting runtime: %w", err)
	}

	fmt.Printf("%s Mayor restarted with context\n", style.Bold.Render("✓"))
	return nil
}

func runMayorStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	mgr := mayor.NewManager(townRoot)
	status, err := mgr.CombinedStatus()
	if err != nil {
		return err
	}

	if mayorStatusRunning {
		fmt.Println(status.Active)
		return nil
	}

	if !status.Active {
		fmt.Printf("%s Mayor session is %s\n",
			style.Dim.Render("○"),
			"not running")
		fmt.Printf("\nStart with: %s\n", style.Dim.Render("gt mayor start"))
		return nil
	}

	if status.Tmux != nil {
		attachedStatus := "detached"
		if status.Tmux.Attached {
			attachedStatus = "attached"
		}
		fmt.Printf("%s Mayor (tmux) is %s\n",
			style.Bold.Render("●"),
			style.Bold.Render("running"))
		fmt.Printf("  Status: %s\n", attachedStatus)
		fmt.Printf("  Created: %s\n", status.Tmux.Created)
	}

	if status.Tmux != nil {
		fmt.Printf("\nAttach with: %s\n", style.Dim.Render("gt mayor attach"))
	}

	return nil
}

func runMayorRestart(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	// Stop if running (ignore not-running error)
	if err := mgr.Stop(); err != nil && err != mayor.ErrNotRunning {
		return fmt.Errorf("stopping session: %w", err)
	}

	// Start fresh
	return runMayorStart(cmd, args)
}

// ensureMayorInfra checks that daemon and dolt are running before attaching
// to the Mayor session. Warns and auto-starts each if absent.
// Returns an error if Dolt fails to start — a missing Dolt server is fatal
// for the Mayor (it cannot operate without database access).
// Daemon failures are non-fatal (warned but do not block).
func ensureMayorInfra(townRoot string) error {
	// Load daemon.json env vars (e.g., GT_DOLT_PORT) so Dolt uses the right port.
	if patrolCfg := daemon.LoadPatrolConfig(townRoot); patrolCfg != nil {
		for k, v := range patrolCfg.Env {
			os.Setenv(k, v)
		}
	}

	// Daemon (non-fatal)
	daemonRunning, _, _ := daemon.IsRunning(townRoot)
	if !daemonRunning {
		style.PrintWarning("daemon is not running, starting...")
		// The supervisor-file note ensureDaemon can return is about a daemon
		// that was already up, which this call site has already ruled out.
		if _, err := ensureDaemon(townRoot); err != nil {
			style.PrintWarning("daemon start failed: %v", err)
		} else {
			fmt.Printf("  %s Daemon started\n", style.Bold.Render("✓"))
		}
	}

	// Dolt (fatal on failure — Mayor requires database access)
	doltCfg := doltserver.DefaultConfig(townRoot)
	if !doltCfg.IsRemote() {
		if _, err := os.Stat(doltCfg.DataDir); err == nil {
			doltRunning, _, _ := doltserver.IsRunning(townRoot)
			if !doltRunning {
				style.PrintWarning("Dolt server is not running, starting...")
				if err := doltserver.Start(townRoot); err != nil {
					// Enrich port-conflict errors with a concrete free-port suggestion.
					msg := fmt.Sprintf("Dolt server start failed: %v", err)
					if pid, dataDir := doltserver.PortHolder(doltCfg.Port); pid > 0 {
						if dataDir != "" {
							msg += fmt.Sprintf("\n  port %d held by dolt PID %d serving %s", doltCfg.Port, pid, dataDir)
						} else {
							msg += fmt.Sprintf("\n  port %d held by PID %d", doltCfg.Port, pid)
						}
					}
					if freePort := doltserver.FindFreePort(doltCfg.Port + 1); freePort > 0 {
						msg += fmt.Sprintf("\n\nConfigure a free port for this town, then retry:\n  gt config set dolt.port %d && gt mayor at", freePort)
					}
					return fmt.Errorf("%s", msg)
				}
				fmt.Printf("  %s Dolt server started (port %d)\n", style.Bold.Render("✓"), doltCfg.Port)
			}
		}
	}
	return nil
}
