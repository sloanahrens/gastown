// poller.go provides a background heartbeat poller for the Deacon session.
//
// touchDeaconHeartbeat (internal/cmd/root.go persistentPreRun, gt-13z) only
// refreshes deacon/heartbeat.json when a `gt` command runs. A patrol step
// that spends a long stretch on bd/git/grep investigation without invoking
// `gt` leaves the heartbeat stale for the whole stretch, risking the
// daemon's very-stale kill even though the Deacon is actively working
// (gt-x8y: gt-13z's fix covers steps incidentally, via the gt/bd commands
// those steps happen to run, not because liveness is decoupled from step
// duration as originally specified).
//
// This poller closes that gap: a detached background process ticks on its
// own interval — independent of any gt/bd invocation — and touches the
// heartbeat as long as the Deacon's tmux session is alive.
//
// Lifecycle: StartHeartbeatPoller() -> background loop -> StopHeartbeatPoller()
// (or session death, detected by the loop itself). Mirrors internal/nudge's
// poller (PID file, detached process group, SIGTERM to stop).
package deacon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// DefaultHeartbeatPollInterval is how often the background poller refreshes
// the heartbeat file. Comfortably under HeartbeatStaleThreshold (5m) so a
// live Deacon never reads as stale merely because it ran no gt commands.
var DefaultHeartbeatPollInterval = "3m"

func heartbeatPollerPidDir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "deacon_heartbeat_poller")
}

func heartbeatPollerPidFile(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(heartbeatPollerPidDir(townRoot), safe+".pid")
}

// StartHeartbeatPoller launches a background `gt deacon heartbeat-poller
// <session>` process. The process is detached (Setpgid) so it survives the
// caller's exit. Returns the PID of the launched process, or an error.
func StartHeartbeatPoller(townRoot, session string) (int, error) {
	pidDir := heartbeatPollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		return 0, fmt.Errorf("creating heartbeat poller pid dir: %w", err)
	}

	// Check if a poller is already running for this session.
	if pid, alive := heartbeatPollerAlive(townRoot, session); alive {
		return pid, nil // already running
	}

	gtBin, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("finding gt binary: %w", err)
	}

	cmd := buildHeartbeatPollerCommand(gtBin, townRoot, session)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting deacon heartbeat-poller: %w", err)
	}

	pid := cmd.Process.Pid

	pidPath := heartbeatPollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		// Non-fatal — the process is running, we just can't track it.
		fmt.Fprintf(os.Stderr, "Warning: failed to write heartbeat poller PID file: %v\n", err)
	}

	// Release the process so it runs independently.
	_ = cmd.Process.Release()

	return pid, nil
}

func buildHeartbeatPollerCommand(gtBin, townRoot, session string) *exec.Cmd {
	cmd := exec.Command(gtBin, "deacon", "heartbeat-poller", session)
	cmd.Dir = townRoot
	cmd.Stdout = nil // discard
	cmd.Stderr = nil // discard
	util.SetDetachedProcessGroup(cmd)
	return cmd
}

// StopHeartbeatPoller terminates the heartbeat poller for a session, if running.
func StopHeartbeatPoller(townRoot, session string) error {
	pidPath := heartbeatPollerPidFile(townRoot, session)

	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no poller to stop
		}
		return fmt.Errorf("reading heartbeat poller PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		_ = os.Remove(pidPath)
		return nil // corrupt PID file, clean up
	}

	if !heartbeatPollerProcessAlive(pid) {
		// Process already dead.
		_ = os.Remove(pidPath)
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(pidPath)
		return nil
	}

	// Send SIGTERM for graceful shutdown.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = os.Remove(pidPath)
		return fmt.Errorf("sending SIGTERM to heartbeat poller (pid %d): %w", pid, err)
	}

	_ = os.Remove(pidPath)
	return nil
}

// HeartbeatPollerStatus reports whether a heartbeat poller is currently
// running for the given session, and its PID if so. Exposed for `gt deacon
// status` and similar diagnostics (gt-nrl: no command previously reported
// poller liveness, so confirming the gt-x8y protection was armed required
// reading source, grepping ps, and instrumenting heartbeat.json by hand).
func HeartbeatPollerStatus(townRoot, session string) (pid int, alive bool) {
	return heartbeatPollerAlive(townRoot, session)
}

// heartbeatPollerAlive checks if a poller is running for the given session.
// Returns the PID and whether the process is alive.
func heartbeatPollerAlive(townRoot, session string) (int, bool) {
	pidPath := heartbeatPollerPidFile(townRoot, session)

	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}

	if !heartbeatPollerProcessAlive(pid) {
		// Stale PID file — clean up.
		_ = os.Remove(pidPath)
		return 0, false
	}

	return pid, true
}
