// poller.go provides a background nudge-queue poller for agents that lack
// turn-boundary drain hooks (e.g., Gemini, Codex). Claude Code drains its
// queue via the UserPromptSubmit hook on every turn. Other runtimes have no
// equivalent hook, so queued nudges would sit undelivered forever.
//
// The poller runs as a background goroutine launched by crew/manager.Start().
// It polls the queue every PollInterval, waits for the agent to be idle, then
// drains and injects the formatted nudges via tmux NudgeSession.
//
// Lifecycle: StartPoller() → background loop → StopPoller() (or session death).
// A PID file at <townRoot>/.runtime/nudge_poller/<session>.pid allows Stop()
// to clean up even if the original manager has been replaced. It holds a
// procid.ID (pid plus kernel start token), never a bare pid: a recycled pid
// would make a dead poller look alive, and StopPoller would SIGTERM whatever
// process got the number (deep review G1-05).
package nudge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/procid"
	"github.com/steveyegge/gastown/internal/util"
)

// Poller tuning defaults (overridable via flags or tests).
var (
	// DefaultPollInterval is how often the poller checks the queue.
	DefaultPollInterval = "10s"
	// DefaultIdleTimeout is how long to wait for the agent to become idle
	// before skipping this poll cycle and trying again next interval.
	DefaultIdleTimeout = "3s"
)

// pollerPidDir returns the directory for poller PID files.
func pollerPidDir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_poller")
}

// pollerPidFile returns the PID file path for a session's poller.
func pollerPidFile(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(pollerPidDir(townRoot), safe+".pid")
}

// pollerProcs holds the process operations the pid file depends on, so tests
// can script a process table (including a recycled pid) instead of probing
// real processes.
type pollerProcs struct {
	token     func(pid int) (string, bool)
	terminate func(pid int) error
}

// osPollerProcs reads and signals real processes.
var osPollerProcs = pollerProcs{token: procid.StartToken, terminate: sigterm}

func sigterm(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// StartPoller launches a background `gt nudge-poller <session>` process.
// The process is detached (Setpgid) so it survives the caller's exit.
// Returns the PID of the launched process, or an error.
func StartPoller(townRoot, session string) (int, error) {
	return osPollerProcs.startPoller(townRoot, session, os.Executable)
}

// startPoller is StartPoller with the binary lookup injected, so tests can
// hand it a path without exec'ing anything.
func (pp pollerProcs) startPoller(townRoot, session string, executable func() (string, error)) (int, error) {
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		return 0, fmt.Errorf("creating poller pid dir: %w", err)
	}

	// Check if a poller is already running for this session.
	if pid, alive := pp.pollerAlive(townRoot, session); alive {
		return pid, nil // already running
	}

	// Find the gt binary.
	gtBin, err := executable()
	if err != nil {
		return 0, fmt.Errorf("finding gt binary: %w", err)
	}
	if strings.HasSuffix(gtBin, ".test") || strings.HasSuffix(gtBin, ".test.exe") {
		// Under `go test` the executable is the test binary; exec'ing it as
		// the poller would run the whole suite again, detached, and every
		// StartPoller site in that run would spawn another (same guard as
		// the daemon's boot triage).
		return 0, fmt.Errorf("refusing to run %s as nudge-poller: executable is a test binary", filepath.Base(gtBin))
	}

	cmd := buildPollerCommand(gtBin, townRoot, session)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting nudge-poller: %w", err)
	}

	pid := cmd.Process.Pid

	// Record the poller's identity for later cleanup. Without a start token
	// nothing could ever match the record, so none is written.
	token, ok := pp.token(pid)
	if !ok || token == "" {
		// Non-fatal — the process is running, we just can't track it.
		fmt.Fprintf(os.Stderr, "Warning: cannot read start time of poller (pid %d); not tracking it\n", pid)
	} else if err := os.WriteFile(pollerPidFile(townRoot, session), []byte(procid.ID{PID: pid, Start: token}.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write poller PID file: %v\n", err)
	}

	// Release the process so it runs independently.
	_ = cmd.Process.Release()

	return pid, nil
}

func buildPollerCommand(gtBin, townRoot, session string) *exec.Cmd {
	cmd := exec.Command(gtBin, "nudge-poller", session)
	cmd.Dir = townRoot
	cmd.Stdout = nil // discard
	cmd.Stderr = nil // discard
	util.SetDetachedProcessGroup(cmd)
	return cmd
}

// StopPoller terminates the nudge-poller for a session, if running.
func StopPoller(townRoot, session string) error {
	return osPollerProcs.stopPoller(townRoot, session)
}

func (pp pollerProcs) stopPoller(townRoot, session string) error {
	pidPath := pollerPidFile(townRoot, session)
	pid, alive := pp.pollerAlive(townRoot, session)
	if !alive {
		// No record, or one that no longer names our poller (dead, pid
		// reused, or a bare pid from before records carried a start time).
		// pollerAlive has already removed a stale file.
		return nil
	}

	// Send SIGTERM for graceful shutdown.
	if err := pp.terminate(pid); err != nil {
		_ = os.Remove(pidPath)
		return fmt.Errorf("sending SIGTERM to poller (pid %d): %w", pid, err)
	}

	_ = os.Remove(pidPath)
	return nil
}

// pollerAlive checks if a poller is running for the given session.
// Returns the PID and whether the process is alive. A record that does not
// name the same live process (missing start time, dead pid, or a pid reused by
// another process) is stale: it is removed and reported not alive.
func (pp pollerProcs) pollerAlive(townRoot, session string) (int, bool) {
	pidPath := pollerPidFile(townRoot, session)

	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}

	id, err := procid.Parse(string(data))
	if err != nil || !id.Running(pp.token) {
		_ = os.Remove(pidPath)
		return 0, false
	}

	return id.PID, true
}
