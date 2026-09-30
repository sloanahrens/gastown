package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// DaemonCheck verifies the daemon is running.
type DaemonCheck struct {
	FixableCheck
}

// NewDaemonCheck creates a new daemon check.
func NewDaemonCheck() *DaemonCheck {
	return &DaemonCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "daemon",
				CheckDescription: "Check if Gas Town daemon is running",
				CheckCategory:    CategoryInfrastructure,
			},
		},
	}
}

// daemonJobState reads the supervisor job's live state; a seam so tests need
// not ask the host's service manager.
var daemonJobState templates.SupervisorReader = templates.SupervisorJobState

func pidIfRunning(running bool, pid int) int {
	if running {
		return pid
	}
	return 0
}

// Run checks if the daemon is running.
func (c *DaemonCheck) Run(ctx *CheckContext) *CheckResult {
	running, pid, err := daemon.IsRunning(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to check daemon status",
			Details: []string{err.Error()},
		}
	}

	// A provisioned job the service manager no longer knows is a failure
	// whether or not a daemon is up: nothing restarts the daemon if it dies,
	// and a hand-started one beside the job is the crash loop gt-3jrm closed
	// (gt-4k3fj.11).
	if kind, missing := templates.SupervisorJobMissing(ctx.TownRoot, daemonJobState); missing {
		msg := "Daemon supervisor job is not loaded (" + kind + ")"
		if running {
			msg += "; daemon PID " + strconv.Itoa(pid) + " is unsupervised"
		} else {
			msg += " and the daemon is not running"
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: msg,
			Details: []string{"Supervised: " + templates.SupervisorStatusLine(ctx.TownRoot, pidIfRunning(running, pid), daemonJobState)},
			FixHint: "Run 'gt doctor --fix' (loads the job through gt daemon start/restart)",
		}
	}

	if running {
		// Get more info about daemon state
		state, err := daemon.LoadState(ctx.TownRoot)
		details := []string{}
		if err == nil && !state.StartedAt.IsZero() {
			uptime := time.Since(state.StartedAt).Round(time.Second)
			details = append(details, "Uptime: "+uptime.String())
			if state.HeartbeatCount > 0 {
				details = append(details, "Heartbeats: "+strconv.FormatInt(state.HeartbeatCount, 10))
			}
		}
		details = append(details, "Supervised: "+templates.SupervisorStatusLine(ctx.TownRoot, pid, templates.SupervisorJobState))

		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Daemon is running (PID " + strconv.Itoa(pid) + ")",
			Details: details,
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Daemon is not running",
		Details: []string{"Supervised: " + templates.SupervisorStatusLine(ctx.TownRoot, 0, templates.SupervisorJobState)},
		FixHint: "Run 'gt daemon start' or 'gt doctor --fix'",
	}
}

// Fix starts the daemon.
func (c *DaemonCheck) Fix(ctx *CheckContext) error {
	if ctx.NoStart {
		return ErrSkippedNoStart
	}

	// Find gt executable
	gtPath, err := os.Executable()
	if err != nil {
		return err
	}

	// With a supervisor job provisioned, the daemon is brought up through it
	// by gt daemon start / restart — a hand spawn beside a job that is not
	// loaded leaves the daemon unsupervised (gt-4k3fj.11). Restart is the
	// one that also replaces a daemon already up outside the job.
	if _, missing := templates.SupervisorJobMissing(ctx.TownRoot, daemonJobState); missing {
		verb := "start"
		if running, _, err := daemon.IsRunning(ctx.TownRoot); err == nil && running {
			verb = "restart"
		}
		cmd := exec.Command(gtPath, "daemon", verb)
		cmd.Dir = ctx.TownRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("gt daemon %s: %w: %s", verb, err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	// Start daemon in background (detach from parent I/O - daemon uses its own logging)
	cmd := exec.Command(gtPath, "daemon", "run")
	cmd.Dir = ctx.TownRoot
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	// Wait a moment for daemon to initialize
	time.Sleep(300 * time.Millisecond)

	return nil
}
