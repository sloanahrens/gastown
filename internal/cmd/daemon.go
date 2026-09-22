package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/workspace"
)

var daemonCmd = &cobra.Command{
	Use:     "daemon",
	GroupID: GroupServices,
	Short:   "Manage the Gas Town daemon",
	RunE:    requireSubcommand,
	Long: `Manage the Gas Town background daemon.

The daemon is a simple Go process that:
- Pokes agents periodically (heartbeat)
- Processes lifecycle requests (cycle, restart, shutdown)
- Restarts sessions when agents request cycling

The daemon is a "dumb scheduler" - all intelligence is in agents.`,
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	Long: `Start the Gas Town daemon in the background.

The daemon will run until stopped with 'gt daemon stop'.`,
	RunE: runDaemonStart,
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon",
	Long: `Stop the running Gas Town daemon.

The daemon must be running or this command returns an error.

When a supervisor (launchd / systemd) is provisioned for this town, the job is
stopped through it rather than by signaling the process, so the daemon does not
come back; 'gt daemon start' loads the job again. Without one, the daemon
process is signaled and waited for.

Examples:
  gt daemon stop`,
	RunE: runDaemonStop,
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	Long: `Show the current status of the Gas Town daemon.

Displays whether the daemon is running, its PID, uptime, heartbeat
count, and whether the binary has been rebuilt since the daemon started.

Examples:
  gt daemon status`,
	RunE: runDaemonStatus,
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View daemon logs",
	Long: `View the daemon log file.

Shows the most recent log entries from the daemon. Use -n to control
how many lines to display, or -f to follow the log in real time.

Examples:
  gt daemon logs             # Show last 50 lines
  gt daemon logs -n 100      # Show last 100 lines
  gt daemon logs -f           # Follow log output in real time`,
	RunE: runDaemonLogs,
}

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run daemon in foreground (internal)",
	Long: `Run the Gas Town daemon in the foreground.

This is called internally by the daemon start process and supervisor
services (launchd/systemd). Use 'gt daemon start' to start the daemon
normally in the background.`,
	Hidden: true,
	RunE:   runDaemonRun,
}

var daemonEnableSupervisorCmd = &cobra.Command{
	Use:   "enable-supervisor",
	Short: "Configure launchd/systemd for daemon auto-restart",
	Long: `Configure external supervision for the Gas Town daemon.

This command creates and enables a supervisor service (launchd on macOS,
systemd on Linux) that will automatically restart the daemon if it crashes
or terminates. The daemon will also start automatically on login/boot.

Examples:
  gt daemon enable-supervisor    # Configure launchd/systemd`,
	RunE: runDaemonEnableSupervisor,
}

var daemonRotateLogsCmd = &cobra.Command{
	Use:   "rotate-logs",
	Short: "Rotate daemon log files",
	Long: `Rotate all daemon-managed log files.

Uses copytruncate for Dolt server logs (safe for processes with open fds).
daemon.log uses automatic lumberjack rotation and is skipped.

By default, only rotates logs exceeding 100MB. Use --force to rotate all.

Examples:
  gt daemon rotate-logs           # Rotate logs > 100MB
  gt daemon rotate-logs --force   # Rotate all logs regardless of size`,
	RunE: runDaemonRotateLogs,
}

var daemonRotateLogsForce bool

var daemonClearBackoffCmd = &cobra.Command{
	Use:   "clear-backoff <agent>",
	Short: "Clear crash loop backoff for an agent",
	Long: `Clear the crash loop and restart backoff state for an agent.

When an agent crashes repeatedly, the daemon enters crash loop mode and
stops restarting it. Use this command to reset the crash loop counter so
the daemon will resume restarting the agent.

The agent name is the session identity (e.g., "deacon", "mayor").

Examples:
  gt daemon clear-backoff deacon   # Reset deacon crash loop`,
	Args: cobra.ExactArgs(1),
	RunE: runDaemonClearBackoff,
}

var (
	daemonLogLines  int
	daemonLogFollow bool
)

func init() {
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonLogsCmd)
	daemonCmd.AddCommand(daemonRunCmd)
	daemonCmd.AddCommand(daemonEnableSupervisorCmd)
	daemonCmd.AddCommand(daemonClearBackoffCmd)
	daemonCmd.AddCommand(daemonRotateLogsCmd)

	daemonLogsCmd.Flags().IntVarP(&daemonLogLines, "lines", "n", 50, "Number of lines to show")
	daemonLogsCmd.Flags().BoolVarP(&daemonLogFollow, "follow", "f", false, "Follow log output")
	daemonRotateLogsCmd.Flags().BoolVar(&daemonRotateLogsForce, "force", false, "Rotate all logs regardless of size")

	rootCmd.AddCommand(daemonCmd)
}

func runDaemonStart(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	via, pid, err := startDaemon(townRoot)
	if err != nil {
		return err
	}
	if via != "" {
		fmt.Printf("%s Daemon started under %s (PID %d)\n", style.Bold.Render("✓"), via, pid)
	} else {
		fmt.Printf("%s Daemon started (PID %d)\n", style.Bold.Render("✓"), pid)
	}
	return nil
}

// spawnDaemonProcess starts 'gt daemon run' as a detached child of this
// process — the right thing only when no supervisor is provisioned (see
// startDaemon) — and returns the PID of the daemon now holding the lock
// (ours, or a concurrent starter's that won the race).
func spawnDaemonProcess(townRoot string) (int, error) {
	// Start daemon in background
	// We use 'gt daemon run' as the actual daemon process
	gtPath, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("finding executable: %w", err)
	}

	daemonCmd := exec.Command(gtPath, "daemon", "run")
	daemonCmd.Dir = townRoot

	// Detach from terminal
	daemonCmd.Stdin = nil
	daemonCmd.Stdout = nil
	daemonCmd.Stderr = nil
	util.SetDetachedProcessGroup(daemonCmd)

	if err := daemonCmd.Start(); err != nil {
		return 0, fmt.Errorf("starting daemon: %w", err)
	}

	// Poll for daemon to initialize and acquire the lock (up to 3s). If a
	// concurrent starter won the race our child exited without the lock and
	// the PID file names the winner — that daemon is as good as ours.
	pid, err := waitForDaemon(townRoot)
	if err != nil {
		if msg := readDaemonStartupFailure(townRoot, daemonCmd.Process.Pid); msg != "" {
			return 0, fmt.Errorf("daemon failed to start: %s", msg)
		}
		return 0, err
	}
	return pid, nil
}

func runDaemonStop(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	running, pid, err := daemonIsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	// A signal alone does not stop a supervised daemon: the loaded KeepAlive
	// job respawns the process it manages, and one it does not manage takes
	// the lock the moment the signal lands. Stop the job through the
	// supervisor so the job is left unloaded, then mop up whatever still
	// holds the lock (gt-sq9e).
	//
	// A probe or bootout that fails leaves the job's state unestablished, and
	// "a signal was sent" is not "the daemon is stopped": that case is carried
	// out of here as an error, after the mop-up has run, so the operator gets
	// the uncertainty and the mechanism that was used instead (gt-ojbb).
	stoppedUnder := ""
	var unconfirmed error
	sup, supErr := detectDaemonSupervisor(townRoot)
	switch {
	case supErr != nil:
		unconfirmed = fmt.Errorf("could not tell whether a supervisor is provisioned for this town: %w", supErr)
	case sup != nil:
		switch st := supervisorStateFor(sup.name); {
		case st.Err != nil:
			unconfirmed = fmt.Errorf("could not read the %s job's state: %w", sup.name, st.Err)
		case st.Loaded:
			if runErr := supervisorRun(sup.stop); runErr != nil {
				unconfirmed = fmt.Errorf("could not stop the %s job: %w", sup.name, runErr)
			} else {
				stoppedUnder = sup.name
			}
		}
	}

	// Whatever the supervisor left behind — a daemon it does not manage, or a
	// job that was never loaded — the lock holder has to go before this can
	// report a stop.
	outcome := "the daemon is gone from daemon.lock"
	stillRunning, _, err := daemonIsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status after stopping: %w", err)
	}
	if stillRunning {
		outcome = "the daemon was signaled directly"
		if err := stopDaemonDirect(townRoot); err != nil {
			// The supervisor's SIGTERM can land between that re-check and
			// StopDaemon's own: the process releases the lock and StopDaemon
			// finds nothing to stop. The stop this path is after still
			// happened, so a free lock is the outcome, not an error (gt-ojbb).
			nowRunning, _, checkErr := daemonIsRunning(townRoot)
			if checkErr != nil || nowRunning {
				return fmt.Errorf("stopping daemon: %w", err)
			}
			outcome = "the daemon released the lock while the stop was in progress"
		}
	}

	if unconfirmed != nil {
		return fmt.Errorf("%w\n  %s, but only the supervisor can leave a KeepAlive job stopped: it may respawn the daemon. Check with: %s",
			unconfirmed, outcome, style.Dim.Render("gt daemon status"))
	}

	if stoppedUnder != "" {
		fmt.Printf("%s Daemon stopped (was PID %d) — the %s job is stopped, so it stays down\n",
			style.Bold.Render("✓"), pid, stoppedUnder)
		fmt.Printf("  Start it again with: %s\n", style.Dim.Render("gt daemon start"))
		return nil
	}
	fmt.Printf("%s Daemon stopped (was PID %d)\n", style.Bold.Render("✓"), pid)
	return nil
}

func runDaemonStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	running, pid, err := daemonIsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}

	if running {
		fmt.Printf("%s Daemon is %s (PID %d)\n",
			style.Bold.Render("●"),
			style.Bold.Render("running"),
			pid)
		fmt.Printf("  Town: %s\n", townRoot)
		// Reports on the supervisor's own process, not just its file: a daemon
		// holding the lock while the provisioned job crash-loops behind it is
		// the state this line has to name (gt-sq9e).
		fmt.Printf("  Supervised: %s\n", templates.SupervisorStatusLine(townRoot, pid, supervisorStateFor))

		// Load state for more details
		state, err := daemon.LoadState(townRoot)
		if err == nil && !state.StartedAt.IsZero() {
			fmt.Printf("  Started: %s\n", state.StartedAt.Format("2006-01-02 15:04:05"))
			if !state.LastHeartbeat.IsZero() {
				fmt.Printf("  Last heartbeat: %s (#%d)\n",
					state.LastHeartbeat.Format("15:04:05"),
					state.HeartbeatCount)
			}

			// Check if binary is newer than process
			if binaryModTime, err := getBinaryModTime(); err == nil {
				fmt.Printf("  Binary: %s\n", binaryModTime.Format("2006-01-02 15:04:05"))
				if binaryModTime.After(state.StartedAt) {
					fmt.Printf("  %s Binary is newer than process - consider '%s'\n",
						style.Bold.Render("⚠"),
						style.Dim.Render("gt daemon stop && gt daemon start"))
				}
			}
		}
	} else {
		fmt.Printf("%s Daemon is %s\n",
			style.Dim.Render("○"),
			"not running")
		fmt.Printf("  Supervised: %s\n", templates.SupervisorStatusLine(townRoot, 0, supervisorStateFor))
		fmt.Printf("\nStart with: %s\n", style.Dim.Render("gt daemon start"))
	}

	return nil
}

// getBinaryModTime returns the modification time of the current executable
func getBinaryModTime() (time.Time, error) {
	exePath, err := os.Executable()
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(exePath)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

func readDaemonStartupFailure(townRoot string, pid int) string {
	logFile := filepath.Join(townRoot, "daemon", "daemon.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		return ""
	}

	prefix := fmt.Sprintf("Daemon startup failed (PID %d): ", pid)
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if idx := strings.Index(line, prefix); idx >= 0 {
			return strings.TrimSpace(line[idx+len(prefix):])
		}
	}
	return ""
}

func runDaemonLogs(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	logFile := filepath.Join(townRoot, "daemon", "daemon.log")

	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		return fmt.Errorf("no log file found at %s", logFile)
	}

	if daemonLogFollow {
		// Use tail -f for following
		tailCmd := exec.Command("tail", "-f", logFile)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		return tailCmd.Run()
	}

	// Use tail -n for last N lines
	tailCmd := exec.Command("tail", "-n", fmt.Sprintf("%d", daemonLogLines), logFile)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	return tailCmd.Run()
}

func runDaemonRun(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Clear agent identity env vars inherited from the launch environment.
	// When the daemon is started from an agent session (e.g. crew runs
	// 'gt daemon start'), it inherits GT_ROLE/GT_CREW/etc. Any subprocess
	// that derives sender identity from ambient env vars (e.g. gt mail send)
	// would then be misattributed to the launching agent. GH#3006.
	for _, k := range agentconfig.IdentityEnvVars {
		os.Unsetenv(k)
	}
	os.Setenv("BD_ACTOR", "daemon")

	config := daemon.DefaultConfig(townRoot)
	d, err := daemon.New(config)
	if err != nil {
		return fmt.Errorf("creating daemon: %w", err)
	}

	return d.Run()
}

func runDaemonEnableSupervisor(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Refuse while a manual daemon holds the lock. RunAtLoad + KeepAlive.Crashed
	// means launchd would immediately spawn a second daemon that loses the
	// flock on daemon.lock and exits non-zero, then gets respawned every ~10s
	// for as long as the manual daemon lives.
	running, _, err := daemon.IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}
	if running {
		return fmt.Errorf("a daemon is already running — stop the running daemon first: gt daemon stop")
	}

	msg, err := templates.ProvisionSupervisor(townRoot)
	if err != nil {
		return fmt.Errorf("configuring supervisor: %w", err)
	}

	fmt.Printf("%s %s\n", style.Bold.Render("✓"), msg)
	fmt.Println("\nThe daemon will now:")
	fmt.Println("  - Auto-restart if it crashes")
	fmt.Println("  - Start automatically on login/boot")
	fmt.Println("\nTo stop the supervised daemon:")
	fmt.Println("  gt daemon stop")
	return nil
}

func runDaemonClearBackoff(cmd *cobra.Command, args []string) error {
	agentID := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Clear the crash loop state on disk
	if err := daemon.ClearAgentBackoff(townRoot, agentID); err != nil {
		return fmt.Errorf("clearing backoff for %s: %w", agentID, err)
	}

	// Signal the daemon to reload its in-memory restart tracker
	running, pid, err := daemon.IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}
	if running {
		process, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("finding daemon process: %w", err)
		}
		if err := signalDaemonReload(process); err != nil {
			return fmt.Errorf("signaling daemon to reload: %w", err)
		}
		fmt.Printf("%s Cleared backoff for %s (daemon reloaded)\n", style.Bold.Render("✓"), agentID)
	} else {
		fmt.Printf("%s Cleared backoff for %s (daemon not running, will take effect on next start)\n",
			style.Bold.Render("✓"), agentID)
	}

	return nil
}

func runDaemonRotateLogs(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	var result *daemon.RotateLogsResult
	if daemonRotateLogsForce {
		result = daemon.ForceRotateLogs(townRoot)
	} else {
		result = daemon.RotateLogs(townRoot)
	}

	for _, path := range result.Rotated {
		fmt.Printf("%s Rotated %s\n", style.Bold.Render("✓"), path)
	}
	for _, path := range result.Skipped {
		fmt.Printf("  %s %s (below threshold)\n", style.Dim.Render("·"), path)
	}
	for _, err := range result.Errors {
		fmt.Printf("  %s %v\n", style.Warning.Render("⚠"), err)
	}

	if len(result.Rotated) == 0 && len(result.Errors) == 0 {
		fmt.Printf("%s No logs needed rotation\n", style.Bold.Render("✓"))
	}

	return nil
}
