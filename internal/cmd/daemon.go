package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
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

The daemon will run until stopped with 'gt daemon stop'.

Exit codes of 'gt daemon run': 0 after 'gt daemon stop' or 'gt down'; 75 after
an upgrade restart or after any signal gt did not send, so a launchd job with
KeepAlive SuccessfulExit=false relaunches it.`,
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

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon on the binary now on disk",
	Long: `Restart the Gas Town daemon.

Use this after replacing the gt binary: the daemon is a long-running process,
so it keeps executing the program it was started from — including the script
plugins it runs in-process — until it is restarted. A restart that finds no
daemon running starts one.

When a supervisor (launchd / systemd) is provisioned for this town, the job is
restarted through it (launchctl kickstart -k), which reloads the program the
job points at. Do not use 'gt daemon stop' followed by 'gt daemon start' for
this: stop unloads the job, so the daemon is unsupervised until the start
lands and stays down if that start fails.

Examples:
  gt daemon restart`,
	RunE: runDaemonRestart,
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	Long: `Show the current status of the Gas Town daemon.

Displays whether the daemon is running, its PID, uptime, heartbeat
count, and whether the binary has been rebuilt since the daemon started.
A town whose daemon runs under the container supervisor reads running
from that container and daemon/state.json, and has no PID to show.

Exits 0 when the daemon is running and 3 when it is not, so scripts can
use it as a running check (for example: gt daemon status || gt daemon start).

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

The agent name is the session identity (e.g., "gastown/witness").

Examples:
  gt daemon clear-backoff deacon   # Reset deacon crash loop`,
	Args: cobra.ExactArgs(1),
	RunE: runDaemonClearBackoff,
}

var (
	daemonLogLines  int
	daemonLogFollow bool
	// daemonStartTakeover is gt daemon start --takeover.
	daemonStartTakeover bool
)

func init() {
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonLogsCmd)
	daemonCmd.AddCommand(daemonRunCmd)
	daemonCmd.AddCommand(daemonEnableSupervisorCmd)
	daemonCmd.AddCommand(daemonClearBackoffCmd)
	daemonCmd.AddCommand(daemonRotateLogsCmd)
	daemonCmd.AddCommand(daemonContainerEntrypointCmd)

	daemonStartCmd.Flags().BoolVar(&daemonStartTakeover, "takeover", false, "Start over a recorded owner whose heartbeat is still fresh, when that owner is shown not running (container and unsupervised starts only)")
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
	via, pid, err := realDaemonControl().startDaemonWith(townRoot, daemonStartTakeover)
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

func runDaemonRestart(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	via, pid, err := restartDaemon(townRoot)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s\n", style.Bold.Render("✓"), restartedLine(via, pid))
	return nil
}

// restartedLine is what gt daemon restart prints. A container restart has no
// pid to report: the lock pid of a container is from its own namespace, and the
// restart is proven by state.json instead.
func restartedLine(via string, pid int) string {
	line := "Daemon restarted"
	if via != "" {
		line += " under " + via
	}
	if pid > 0 {
		line += fmt.Sprintf(" (PID %d)", pid)
	}
	return line
}

// spawnDaemonProcess starts 'gt daemon run' as a detached child of this
// process — the right thing only when no supervisor is provisioned (see
// startDaemon) — and returns the PID of the daemon now holding the lock
// (ours, or a concurrent starter's that won the race). env is added to the
// child's environment.
func spawnDaemonProcess(c *daemonControl, townRoot string, env []string) (int, error) {
	// Start daemon in background
	// We use 'gt daemon run' as the actual daemon process
	gtPath, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("finding executable: %w", err)
	}

	daemonCmd := exec.Command(gtPath, "daemon", "run")
	daemonCmd.Dir = townRoot
	if len(env) > 0 {
		// --takeover: the child's own startup guard reads the variable.
		daemonCmd.Env = append(os.Environ(), env...)
	}

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
	pid, err := c.waitForDaemon(townRoot)
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
	return realDaemonControl().stopDaemon(cmd.OutOrStdout(), townRoot)
}

// stopDaemon stops townRoot's daemon and leaves its supervisor job, if any,
// stopped, reporting to w.
func (c *daemonControl) stopDaemon(w io.Writer, townRoot string) error {
	running, pid, err := c.isRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	// Whether this town runs in a container decides whether the direct signal
	// below is allowed at all, and it is read from the settings rather than the
	// detected supervisor because detection can fail where the kind still
	// decides that: a compose file that went missing is a detection error, and
	// it is not a license to signal a host PID.
	cfg, containerTown, settingsErr := c.townDaemonKind(townRoot)

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
	// absentContainer names the container docker reported as absent, if it
	// did: nothing of this town runs there, so the lock is stale and no
	// signal is owed to its PID (gt-qpeei.18).
	absentContainer := ""
	var unconfirmed error
	sup, supErr := c.detectDaemonSupervisor(townRoot)
	switch {
	case supErr != nil:
		unconfirmed = fmt.Errorf("could not tell whether a supervisor is provisioned for this town: %w", supErr)
	case sup != nil:
		switch st := sup.jobState(); {
		case st.Err != nil:
			unconfirmed = fmt.Errorf("could not read the %s job's state: %w", sup.name, st.Err)
		case sup.name == config.SupervisorContainer && !st.Loaded:
			absentContainer = st.Container
		case st.Loaded:
			if runErr := sup.runArgv(sup.stop); runErr != nil {
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
	stillRunning, _, err := c.isRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status after stopping: %w", err)
	}
	if stillRunning {
		// A container town's daemon runs in another namespace, so the lock's
		// PID is not a host process to signal: the stop is its supervisor's
		// argv or an error (containerStopError), never the mop-up the launchd
		// and systemd kinds may fall back on (gt-qpeei.10).
		if containerTown {
			if absentContainer != "" {
				return fmt.Errorf("daemon.lock holds PID %d, but docker reports no container %s: the daemon is not running there, so the lock is stale and its PID was not signaled", pid, absentContainer)
			}
			return c.containerStopError(townRoot, cfg, sup, unconfirmed)
		}
		if settingsErr != nil {
			return fmt.Errorf("the daemon is still running, and the town's daemon settings could not be read to say whether it runs in a container, so it was not signaled: %w", settingsErr)
		}
		outcome = "the daemon was signaled directly"
		if err := c.stopDirect(townRoot); err != nil {
			// The supervisor's SIGTERM can land between that re-check and
			// StopDaemon's own: the process releases the lock and StopDaemon
			// finds nothing to stop. The stop this path is after still
			// happened, so a free lock is the outcome, not an error (gt-ojbb).
			nowRunning, _, checkErr := c.isRunning(townRoot)
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
		fmt.Fprintf(w, "%s Daemon stopped (was PID %d) — the %s job is stopped, so it stays down\n",
			style.Bold.Render("✓"), pid, stoppedUnder)
		fmt.Fprintf(w, "  Start it again with: %s\n", style.Dim.Render("gt daemon start"))
		return nil
	}
	fmt.Fprintf(w, "%s Daemon stopped (was PID %d)\n", style.Bold.Render("✓"), pid)
	return nil
}

func runDaemonStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return realDaemonControl().daemonStatus(cmd.OutOrStdout(), townRoot)
}

// daemonStatus reports townRoot's daemon and its supervisor to w, and exits 3
// when no daemon runs.
func (c *daemonControl) daemonStatus(w io.Writer, townRoot string) error {
	// A container town's daemon lives in another namespace: the flock it takes
	// on the bind-mounted town is not visible from this host, so the lock
	// answers nothing about it (gt-i2z9l). Its container is asked instead.
	// Settings that cannot be read, or name no kind this package knows, keep
	// the lock reading every other town takes.
	cfg, containerTown, settingsErr := c.townDaemonKind(townRoot)
	if settingsErr == nil && containerTown {
		return c.containerDaemonStatus(w, townRoot, cfg)
	}

	running, pid, err := c.isRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}

	if running {
		fmt.Fprintf(w, "%s Daemon is %s (PID %d)\n",
			style.Bold.Render("●"),
			style.Bold.Render("running"),
			pid)
		fmt.Fprintf(w, "  Town: %s\n", townRoot)
		// Reports on the supervisor's own process, not just its file: a daemon
		// holding the lock while the provisioned job crash-loops behind it is
		// the state this line has to name (gt-sq9e).
		fmt.Fprintf(w, "  Supervised: %s\n", c.statusLine(townRoot, pid))

		// Load state for more details
		state, err := daemon.LoadState(townRoot)
		if err == nil && !state.StartedAt.IsZero() {
			fmt.Fprintf(w, "  Started: %s\n", state.StartedAt.Format("2006-01-02 15:04:05"))
			if !state.LastHeartbeat.IsZero() {
				fmt.Fprintf(w, "  Last heartbeat: %s (#%d)\n",
					state.LastHeartbeat.Format("15:04:05"),
					state.HeartbeatCount)
			}

			// Check if binary is newer than process
			if binaryModTime, err := getBinaryModTime(); err == nil {
				fmt.Fprintf(w, "  Binary: %s\n", binaryModTime.Format("2006-01-02 15:04:05"))
				if binaryModTime.After(state.StartedAt) {
					fmt.Fprintf(w, "  %s Binary is newer than process - consider '%s'\n",
						style.Bold.Render("⚠"),
						style.Dim.Render("gt daemon restart"))
				}
			}
		}
	} else {
		return daemonNotRunning(w, c.statusLine(townRoot, 0))
	}

	return nil
}

// daemonNotRunning prints the not-running form of gt daemon status, with the
// Supervised: value the caller read, and is the exit-3 result that form ends
// in. Exit 3 (LSB "program is not running") so scripts can use status as the
// running probe: make install restarts only a running daemon and
// mol-gastown-boot runs `status || start` (gt-o848l).
func daemonNotRunning(w io.Writer, supervised string) error {
	fmt.Fprintf(w, "%s Daemon is %s\n",
		style.Dim.Render("○"),
		"not running")
	fmt.Fprintf(w, "  Supervised: %s\n", supervised)
	fmt.Fprintf(w, "\nStart with: %s\n", style.Dim.Render("gt daemon start"))
	return NewSilentExit(3)
}

// daemonStateUnknown prints what status knows when a read that decides running
// failed, and returns that failure: the daemon may well be up, so neither the
// running nor the not-running form is honest.
func daemonStateUnknown(w io.Writer, supervised string, err error) error {
	fmt.Fprintf(w, "%s Daemon state is %s\n", style.Dim.Render("○"), "unknown")
	fmt.Fprintf(w, "  Supervised: %s\n", supervised)
	return err
}

// containerDaemonStatus is gt daemon status for a town whose daemon runs under
// the container supervisor. The container's live state, together with
// state.json, decides running: the daemon's flock on the bind-mounted town is
// taken in the container's namespace and is not visible from this host, so it
// is never read here (gt-i2z9l). A container state that could not be read, and
// a state.json that could not be, are errors that name what failed — an
// unreadable read is not a daemon that is not running.
func (c *daemonControl) containerDaemonStatus(w io.Writer, townRoot string, cfg *config.DaemonPatrolConfig) error {
	job := c.containerJobState(cfg)(config.SupervisorContainer)
	if job.Err != nil {
		return daemonStateUnknown(w, job.StatusLine(0), fmt.Errorf("checking the daemon's container: %w", job.Err))
	}
	if !job.Loaded || job.Status != "running" {
		return daemonNotRunning(w, job.StatusLine(0))
	}

	read := c.readState
	if read == nil {
		read = readCutoverState
	}
	state, err := read(townRoot)
	if err != nil {
		return daemonStateUnknown(w, job.StatusLine(0), fmt.Errorf("reading state.json: %w", err))
	}
	// The container up is not the daemon up: state.json is where the daemon in
	// it records that it runs, and a container whose daemon has not recorded a
	// run yet runs none.
	if state == nil || !state.Running {
		return daemonNotRunning(w, job.StatusLine(0))
	}

	fmt.Fprintf(w, "%s Daemon is %s\n",
		style.Bold.Render("●"),
		style.Bold.Render("running"))
	fmt.Fprintf(w, "  Town: %s\n", townRoot)
	fmt.Fprintf(w, "  Supervised: %s\n", job.StatusLine(0))
	if !state.StartedAt.IsZero() {
		fmt.Fprintf(w, "  Started: %s\n", state.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if !state.LastHeartbeat.IsZero() {
		fmt.Fprintf(w, "  Last heartbeat: %s (#%d)\n",
			state.LastHeartbeat.Format("15:04:05"),
			state.HeartbeatCount)
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

// daemonRunActor is the identity of the daemon process and of everything it
// spawns.
const daemonRunActor = "daemon"

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
	daemon.PublishIdentity()

	config := daemon.DefaultConfig(townRoot)
	d, err := daemon.New(config)
	if err != nil {
		return fmt.Errorf("creating daemon: %w", err)
	}

	// The heartbeat's dispatch tick runs the scheduler in process (gt-638go.9).
	// The sling and the polecat-capacity probe it needs live in this package,
	// so the one construction site hands them over.
	d.SetSchedulerDeps(schedulerDeps())

	return daemonRunExit(d.Run(), os.Exit)
}

func runDaemonEnableSupervisor(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	c := realDaemonControl()
	return enableSupervisor(townRoot, templates.ProvisionSupervisor, func(townRoot string) error {
		return c.guardStart(townRoot, c.hostSupervisorJob(), false)
	})
}

// enableSupervisor installs the town's supervisor file through provision
// (templates.ProvisionSupervisor) unless a daemon already runs.
//
// guard is the startup guard (gt-qpeei.28): provisioning loads a job with
// RunAtLoad, which starts a daemon, so a town another owner holds is refused
// before the file is written.
func enableSupervisor(townRoot string, provision func(townRoot string, shutdownBudget time.Duration) (string, error), guard func(townRoot string) error) error {
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
	if err := guard(townRoot); err != nil {
		return err
	}

	msg, err := provision(townRoot, daemon.ShutdownBudget)
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
	if err := daemon.ClearAgentBackoff(townRegistry(), townRoot, agentID); err != nil {
		return fmt.Errorf("clearing backoff for %s: %w", agentID, err)
	}

	// Signal the daemon to reload its in-memory restart tracker
	return clearBackoffReload(cmd.OutOrStdout(), agentID, daemonReloadDeps{
		isRunning:   func() (bool, int, error) { return daemon.IsRunning(townRoot) },
		findProcess: os.FindProcess,
		signal:      signalDaemonReload,
	})
}

// daemonReloadDeps is how clear-backoff reads and signals the daemon, so a test
// can drive the pid gate with a fake. Every field must be set.
type daemonReloadDeps struct {
	isRunning   func() (bool, int, error)
	findProcess func(pid int) (*os.Process, error)
	signal      func(process *os.Process) error
}

// clearBackoffReload tells a running daemon that the backoff state it holds in
// memory is stale, and prints what happened. It takes its daemon calls as deps
// so the pid gate around the signal can be driven by a test with a fake daemon.
func clearBackoffReload(w io.Writer, agentID string, deps daemonReloadDeps) error {
	running, pid, err := deps.isRunning()
	if err != nil {
		return fmt.Errorf("checking daemon status: %w", err)
	}
	reload, note := daemonReloadPlan(running, pid)
	if reload {
		process, err := deps.findProcess(pid)
		if err != nil {
			return fmt.Errorf("finding daemon process: %w", err)
		}
		if err := deps.signal(process); err != nil {
			return fmt.Errorf("signaling daemon to reload: %w", err)
		}
	}
	fmt.Fprintf(w, "%s Cleared backoff for %s (%s)\n", style.Bold.Render("✓"), agentID, note)
	return nil
}

// daemonReloadPlan decides whether clear-backoff signals the daemon to reload
// and what the success line says. A running daemon can report no pid to signal:
// a container town's daemon lives in another namespace, and a host town whose
// pid file cannot be read has a lock holder and no process id. Neither pid-below
// -one value may reach os.Process.Signal, where 0 names this process group, so
// both skip the signal and the note names only what the pid tells us.
func daemonReloadPlan(running bool, pid int) (reload bool, note string) {
	switch {
	case !running:
		return false, "daemon not running, will take effect on next start"
	case pid <= 0:
		return false, "daemon reported no pid to signal; the change takes effect when the daemon restarts"
	default:
		return true, "daemon reloaded"
	}
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

// daemonRunExit maps daemon.ErrRestartForUpgrade and daemon.ErrUnrequestedStop
// to exit code 75 so launchd (KeepAlive SuccessfulExit=false) restarts the
// daemon: on the installed binary after an upgrade, or after a signal gt did
// not send. A plain nil return would exit 0 and leave the daemon down. Exit
// codes: 0 = stopped by gt daemon stop / gt down, 75 = relaunch me. exit is
// os.Exit.
func daemonRunExit(err error, exit func(code int)) error {
	if errors.Is(err, daemon.ErrRestartForUpgrade) || errors.Is(err, daemon.ErrUnrequestedStop) {
		exit(75)
		return nil
	}
	return err
}
