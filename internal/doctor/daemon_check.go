package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// DaemonCheck verifies the daemon is running.
type DaemonCheck struct {
	FixableCheck

	// supervisor answers what the host's service manager is doing with the
	// daemon's job; nil is hostSupervisor, which reads the plist / unit file
	// and asks launchctl / systemctl.
	supervisor daemonSupervisor

	// running reports whether a daemon holds the lock, with its pid; nil is
	// daemon.IsRunning. A container town never reaches it, and the container
	// tests replace it with one that fails the test, so a pid read coming
	// back on that path is red (gt-qpeei.20).
	running func(townRoot string) (bool, int, error)
	// containerReading is the container-town reading; nil is
	// readContainerDaemon.
	containerReading func(townRoot string) containerDaemonReading
	// gtCommand runs `gt daemon <verb>` for the repairs that go through a
	// supervisor instead of spawning a process here; nil execs the binary.
	gtCommand func(townRoot, verb string) error
}

// gtDaemon runs `gt daemon <verb>` in townRoot. Its error names the verb and
// not the command's output, which can carry a token (gt-qpeei.20).
func gtDaemon(townRoot, verb string) error {
	gtPath, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(gtPath, "daemon", verb)
	cmd.Dir = townRoot
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gt daemon %s: %w", verb, err)
	}
	return nil
}

// fix runs a repair verb through gtCommand, or the binary when no seam is set.
func (c *DaemonCheck) fix(ctx *CheckContext, verb string) error {
	if c.gtCommand != nil {
		return c.gtCommand(ctx.TownRoot, verb)
	}
	return gtDaemon(ctx.TownRoot, verb)
}

// daemonSupervisor is what DaemonCheck needs from the host's supervisor.
type daemonSupervisor interface {
	// JobMissing reports whether a job is provisioned for the town while the
	// service manager does not know it, and its kind.
	JobMissing(townRoot string) (kind string, missing bool)
	// StatusLine renders the "Supervised:" value for the daemon holding
	// lockPID (0 when none does).
	StatusLine(townRoot string, lockPID int) string
}

// hostSupervisor is the real machine's supervisor.
type hostSupervisor struct{}

func (hostSupervisor) JobMissing(townRoot string) (string, bool) {
	return templates.SupervisorJobMissing(townRoot, templates.SupervisorJobState)
}

func (hostSupervisor) StatusLine(townRoot string, lockPID int) string {
	return templates.SupervisorStatusLine(townRoot, lockPID, templates.SupervisorJobState)
}

func (c *DaemonCheck) sup() daemonSupervisor {
	if c.supervisor != nil {
		return c.supervisor
	}
	return hostSupervisor{}
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

func pidIfRunning(running bool, pid int) int {
	if running {
		return pid
	}
	return 0
}

// Run checks if the daemon is running.
func (c *DaemonCheck) Run(ctx *CheckContext) *CheckResult {
	// A container town's daemon is judged by its container and its own state,
	// never by the pid the lock file names (gt-qpeei.20).
	if v := judgeContainerDaemon(c.Name(), ctx.TownRoot, c.containerReading); v.isContainer {
		if v.result != nil {
			return v.result
		}
		return v.reading.okResult(c.Name())
	}

	isRunning := c.running
	if isRunning == nil {
		isRunning = daemon.IsRunning
	}
	running, pid, err := isRunning(ctx.TownRoot)
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
	if kind, missing := c.sup().JobMissing(ctx.TownRoot); missing {
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
			Details: []string{"Supervised: " + c.sup().StatusLine(ctx.TownRoot, pidIfRunning(running, pid))},
			FixHint: "Run 'gt doctor fix daemon' (loads the job through gt daemon start/restart)",
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
		details = append(details, "Supervised: "+c.sup().StatusLine(ctx.TownRoot, pid))

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
		Details: []string{"Supervised: " + c.sup().StatusLine(ctx.TownRoot, 0)},
		FixHint: "Run 'gt daemon start' or 'gt doctor fix daemon'",
	}
}

// DestructiveFix marks this repair as destructive (gt-638go.3): it restarts (terminates) the running daemon.
func (c *DaemonCheck) DestructiveFix() bool { return true }

// Fix starts the daemon.
func (c *DaemonCheck) Fix(ctx *CheckContext) error {
	if ctx.NoStart {
		return ErrSkippedNoStart
	}

	// A container town's daemon starts only through the container supervisor:
	// `gt daemon run` here would be a second daemon on this host, reading the
	// town beside the container's own (gt-qpeei.20). The daemon's own state
	// picks the verb without a pid read.
	if v := judgeContainerDaemon(c.Name(), ctx.TownRoot, c.containerReading); v.isContainer {
		verb := "start"
		if v.reading.state != nil && v.reading.state.Running {
			verb = "restart"
		}
		return c.fix(ctx, verb)
	}

	// With a supervisor job provisioned, the daemon is brought up through it
	// by gt daemon start / restart — a hand spawn beside a job that is not
	// loaded leaves the daemon unsupervised (gt-4k3fj.11). Restart is the
	// one that also replaces a daemon already up outside the job.
	if _, missing := c.sup().JobMissing(ctx.TownRoot); missing {
		verb := "start"
		if running, _, err := daemon.IsRunning(ctx.TownRoot); err == nil && running {
			verb = "restart"
		}
		return c.fix(ctx, verb)
	}

	// Find gt executable
	gtPath, err := os.Executable()
	if err != nil {
		return err
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
