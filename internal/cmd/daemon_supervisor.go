package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// daemonSupervisor is the service manager a provisioned daemon runs under
// (gt daemon enable-supervisor). Starting the daemon by hand while one of
// these is loaded is the trap gt-3jrm closes: the hand-started process
// holds daemon.lock, and the KeepAlive job then respawns every ~10 s, loses
// the flock and exits 1 for as long as the manual daemon lives —
// runDaemonEnableSupervisor refuses to provision in that state for the same
// reason, and start has to honor the same rule from the other side.
type daemonSupervisor struct {
	name  string   // "launchd" / "systemd", for messages
	start []string // argv that (re)starts the daemon through it
	// stop is argv that stops the daemon through it and leaves the job
	// unloaded, so the daemon stays down: signaling the process alone leaves
	// a KeepAlive job to respawn it (gt-sq9e).
	stop []string
	// bootstrap is argv that loads the job when the service manager does not
	// know it — the state a stop leaves behind on macOS, where bootout
	// unloads and kickstart then cannot reach the job.
	bootstrap []string
	load      string // how to load the job when start says it is not loaded
}

// Seams for tests: supervisor detection, running its command, reading the
// job's live state, the direct spawn, the stop and the running check.
// Production values are the real ones.
var (
	supervisorPlistPath = templates.LaunchdPlistPath
	supervisorUnitPath  = templates.SystemdUnitPath
	supervisorRun       = func(argv []string) error {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv from detectDaemonSupervisor
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	supervisorStateFor templates.SupervisorReader = templates.SupervisorJobState
	spawnDaemonDirect                             = spawnDaemonProcess
	stopDaemonDirect                              = daemon.StopDaemon
	daemonIsRunning                               = daemon.IsRunning
	supervisorGOOS                                = runtime.GOOS
)

// detectDaemonSupervisor reports the supervisor provisioned FOR townRoot, or
// nil when the daemon is meant to be run by hand (no plist / unit file
// installed) or the installed one belongs to another workspace: the file is
// per user, and its WorkingDirectory is the town it was provisioned for —
// kickstarting it from a different town would restart that town's daemon
// and then wait for this town's lock in vain. A file that exists but cannot
// be read is an error, never "no supervisor": the whole point of the check
// is to never hand-spawn beside a provisioned one.
func detectDaemonSupervisor(townRoot string) (*daemonSupervisor, error) {
	switch supervisorGOOS {
	case "darwin":
		p, err := supervisorPlistPath()
		if err != nil {
			// No resolvable home means nowhere a plist could have been
			// provisioned; same reading as templates.SupervisorStatus.
			return nil, nil
		}
		isFor, err := templates.SupervisorFileIsFor(p, townRoot)
		if err != nil || !isFor {
			return nil, err
		}
		return &daemonSupervisor{
			name:      "launchd",
			start:     []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), templates.LaunchdLabel)},
			stop:      []string{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), templates.LaunchdLabel)},
			bootstrap: []string{"launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), p},
			load:      fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), p),
		}, nil
	case "linux":
		p, err := supervisorUnitPath()
		if err != nil {
			return nil, nil
		}
		isFor, err := templates.SupervisorFileIsFor(p, townRoot)
		if err != nil || !isFor {
			return nil, err
		}
		return &daemonSupervisor{
			name:      "systemd",
			start:     []string{"systemctl", "--user", "restart", templates.SystemdUnit},
			stop:      []string{"systemctl", "--user", "stop", templates.SystemdUnit},
			bootstrap: []string{"systemctl", "--user", "enable", "--now", templates.SystemdUnit},
			load:      "systemctl --user daemon-reload && systemctl --user enable --now " + templates.SystemdUnit,
		}, nil
	}
	return nil, nil
}

// startDaemon starts the daemon for townRoot: through the provisioned
// supervisor when there is one, directly otherwise. It reports how the
// daemon was started ("launchd", "systemd" or "" for a direct spawn) and its
// PID; callers print. When the supervisor command fails the daemon is NOT
// spawned by hand — a job that is still loaded would respawn against the
// manual daemon forever, the very loop this path exists to prevent — the
// error says how to load the job, or that removing the file returns the
// daemon to manual operation.
func startDaemon(townRoot string) (via string, pid int, err error) {
	running, pid, err := daemonIsRunning(townRoot)
	if err != nil {
		return "", 0, fmt.Errorf("checking daemon status: %w", err)
	}
	if running {
		return "", pid, fmt.Errorf("daemon already running (PID %d)", pid)
	}
	sup, err := detectDaemonSupervisor(townRoot)
	if err != nil {
		return "", 0, err
	}
	if sup != nil {
		runErr := supervisorRun(sup.start)
		if runErr != nil {
			// kickstart / restart reach only a job the service manager knows.
			// One that gt daemon stop unloaded is bootstrapped instead, so that
			// a stop-then-start pair leaves a supervised daemon rather than the
			// hand spawn that recreates the crash loop (gt-3jrm).
			if st := supervisorStateFor(sup.name); st.Err == nil && !st.Loaded {
				runErr = supervisorRun(sup.bootstrap)
			}
		}
		if runErr != nil {
			// The command may have failed after starting the daemon.
			if pid, err := waitForDaemon(townRoot); err == nil {
				return sup.name, pid, nil
			}
			return sup.name, 0, fmt.Errorf("%s is provisioned for this town but could not start the daemon: %v\n  load it with: %s\n  or remove its file to run the daemon by hand", sup.name, runErr, sup.load)
		}
		pid, err = waitForDaemon(townRoot)
		if err != nil {
			return sup.name, 0, fmt.Errorf("daemon did not come up under %s: %w", sup.name, err)
		}
		return sup.name, pid, nil
	}
	pid, err = spawnDaemonDirect(townRoot)
	return "", pid, err
}

// daemonPollInterval is how often waitForDaemon/waitForRestart re-check the
// lock. A seam so tests can shrink it and finish in milliseconds: the
// attempt counts below are chosen for their real-time products (interval *
// attempts), so shrinking the interval shrinks wall-clock wait time without
// changing how many times the lock gets checked relative to the budget.
var daemonPollInterval = 100 * time.Millisecond

// waitForDaemon polls the lock for up to 3 s and returns the daemon's PID.
func waitForDaemon(townRoot string) (int, error) {
	for range 30 {
		time.Sleep(daemonPollInterval)
		running, pid, err := daemonIsRunning(townRoot)
		if err != nil {
			return 0, fmt.Errorf("checking daemon status: %w", err)
		}
		if running {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon failed to start (check logs with 'gt daemon logs')")
}

// restartDaemon brings the daemon back up on the program now on disk: through
// the provisioned supervisor when there is one, by hand when there is not. It
// is the primitive that makes a freshly installed binary take effect in a
// process that is already running, and it is deliberately NOT a stop
// followed by a start.
//
// On macOS the supervisor's stop is `launchctl bootout`, which unloads the
// job: between a stop and a start the daemon is not supervised at all, so
// anything that fails in that window — a rebuild-gt run killed by the very
// restart it asked for, a crash, a reboot — leaves the town with no daemon
// and no loaded job to bring one back (gt-sq9e). kickstart -k restarts the
// job in place: no unsupervised gap, and no dependence on a second command
// succeeding afterwards.
//
// A daemon that is not running is started rather than restarted, so callers
// that want "the daemon is running the current binary" do not have to check
// first. When no supervisor is provisioned the daemon is only ever run by
// hand, so the hand stop/start pair is the whole of it there.
//
// The outgoing PID is read before the restart so the wait can tell the new
// daemon from the old one: waitForDaemon returns as soon as the lock reads as
// running, and throughout a restart that is true of the process on its way
// out as well.
func restartDaemon(townRoot string) (via string, pid int, err error) {
	running, oldPID, err := daemonIsRunning(townRoot)
	if err != nil {
		return "", 0, fmt.Errorf("checking daemon status: %w", err)
	}
	if !running {
		return startDaemon(townRoot)
	}
	sup, err := detectDaemonSupervisor(townRoot)
	if err != nil {
		return "", 0, err
	}
	if sup == nil {
		if err := stopDaemonDirect(townRoot); err != nil {
			return "", 0, fmt.Errorf("stopping the daemon: %w", err)
		}
		return startDaemon(townRoot)
	}
	runErr := supervisorRun(sup.start)
	if runErr != nil {
		// The command may have failed after the job came up; a live daemon
		// that is not the old one is what the caller asked for either way.
		if pid, waitErr := waitForRestart(townRoot, oldPID); waitErr == nil {
			return sup.name, pid, nil
		}
		return sup.name, 0, fmt.Errorf("%s is provisioned for this town but could not restart the daemon: %v\n  load it with: %s\n  or remove its file to run the daemon by hand", sup.name, runErr, sup.load)
	}
	pid, err = waitForRestart(townRoot, oldPID)
	if err != nil {
		return sup.name, 0, fmt.Errorf("daemon did not come back under %s: %w", sup.name, err)
	}
	return sup.name, pid, nil
}

// waitForRestart polls the lock for up to 60 s and returns the PID of a
// daemon other than oldPID. The longer budget than waitForDaemon's is for the
// restart's extra step: the outgoing process has to release the lock and the
// supervisor has to bring the job back before there is anything to see, and a
// build-ahead-of-start wait that times out early would report a restart that
// did happen as a failure.
//
// 10 s (the original budget) does not cover the outgoing daemon's own
// shutdown (gt-oqbw MAJOR #5): Daemon.shutdown pushes pending Dolt remotes
// (bounded to 20 s, see pushDoltRemotesBounded), stops the Dolt SQL server
// (up to ~5 s) and flushes OTel (up to 5 s) before the process exits and the
// lock is released — and only then can the incoming daemon acquire it and
// run its own preflight (Dolt metadata repair, opening beads stores) before
// waitForDaemon's poll would see it. 60 s covers that chain with real margin
// without leaving a hung restart to poll forever.
func waitForRestart(townRoot string, oldPID int) (int, error) {
	for range 600 {
		time.Sleep(daemonPollInterval)
		running, pid, err := daemonIsRunning(townRoot)
		if err != nil {
			return 0, fmt.Errorf("checking daemon status: %w", err)
		}
		if running && pid != oldPID {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon failed to restart (check logs with 'gt daemon logs')")
}
