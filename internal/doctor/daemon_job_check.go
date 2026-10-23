package doctor

import (
	"fmt"
	"runtime"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// DaemonJobCheck fails when a loaded launchd job has no process and no process
// holds daemon.lock. It judges the job against the lock alone, whatever its
// last exit code (gt-m7c7k).
type DaemonJobCheck struct {
	BaseCheck

	// goos is the host OS; "" is runtime.GOOS. Only darwin has a launchd job.
	goos string
	// running reports whether a process holds daemon.lock; nil is
	// daemon.IsRunning.
	running func(townRoot string) (bool, int, error)
	// job reads the launchd job serving the town; nil is hostDaemonJob, which
	// is 'launchctl print' and nothing else, read-only.
	job func(townRoot string) (st templates.SupervisorState, provisioned bool)
}

// NewDaemonJobCheck creates a new daemon-job check.
func NewDaemonJobCheck() *DaemonJobCheck {
	return &DaemonJobCheck{
		BaseCheck: BaseCheck{
			CheckName:        "daemon-job",
			CheckDescription: "Verify a loaded launchd daemon job is running when no daemon holds the lock",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// hostDaemonJob reads the launchd job for the town: provisioned is false when
// no launchd plist naming this town is installed.
func hostDaemonJob(townRoot string) (templates.SupervisorState, bool) {
	path, kind := templates.SupervisorFilePath()
	if kind != "launchd" {
		return templates.SupervisorState{Kind: kind, LastExit: -1}, false
	}
	isFor, err := templates.SupervisorFileIsFor(path, townRoot)
	if err != nil {
		return templates.SupervisorState{Kind: kind, LastExit: -1, Err: err}, true
	}
	if !isFor {
		return templates.SupervisorState{Kind: kind, LastExit: -1}, false
	}
	return templates.SupervisorJobState(kind), true
}

// Run fails when the job is loaded, no process holds the lock, and the job has
// no process of its own. The repair is the operator's to run: nothing in gt
// boots a job out or in.
func (c *DaemonJobCheck) Run(ctx *CheckContext) *CheckResult {
	goos := c.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" {
		return &CheckResult{Name: c.Name(), Status: StatusSkipped, Message: "launchd is macOS only"}
	}

	isRunning := c.running
	if isRunning == nil {
		isRunning = daemon.IsRunning
	}
	running, pid, err := isRunning(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the daemon lock",
			Details: []string{err.Error()},
		}
	}
	if running {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: fmt.Sprintf("daemon is running (pid %d)", pid)}
	}

	read := c.job
	if read == nil {
		read = hostDaemonJob
	}
	st, provisioned := read(ctx.TownRoot)
	switch {
	case st.Err != nil:
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the supervisor's job state",
			Details: []string{st.Err.Error()},
		}
	case !provisioned || st.Kind != "launchd":
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "no launchd job serves this town"}
	case !st.Loaded:
		// DaemonCheck owns a provisioned job the manager does not know.
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "the launchd job is not loaded"}
	}

	if st.PID != 0 {
		// A process the job has started, whatever the last run exited with, has
		// not taken the lock yet: starting up, which is neither a pass nor the
		// failure this check names.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("launchd runs pid %d but no process holds daemon.lock", st.PID),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("launchd job is loaded but the daemon is not running: no process, last exit code %s, runs %d", daemonJobExit(st), st.Runs),
		Details: []string{"Supervised: " + st.StatusLine(0)},
		FixHint: "Run: " + templates.SupervisorRepair(st.Kind) +
			" — then confirm 'launchctl print' reports a pid",
	}
}

// daemonJobExit renders the job's last exit status; -1 is one launchd did not
// report.
func daemonJobExit(st templates.SupervisorState) string {
	if st.LastExit < 0 {
		return "unreported"
	}
	if st.LastExit == templates.LaunchdExitConfig {
		return fmt.Sprintf("%d (EX_CONFIG)", st.LastExit)
	}
	return fmt.Sprintf("%d", st.LastExit)
}
