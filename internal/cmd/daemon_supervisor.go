package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/style"
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

// supervisorFilePath returns the path of the supervisor file this host would
// use for a daemon and the kind it belongs to ("launchd" / "systemd"), or
// ("", "") on a host with no supported supervisor. The file need not exist.
// Both the detection below and the file reconciliation use it, so the two
// never disagree about which file they are talking about.
func supervisorFilePath() (path, kind string) {
	switch supervisorGOOS {
	case "darwin":
		if p, err := supervisorPlistPath(); err == nil {
			return p, "launchd"
		}
	case "linux":
		if p, err := supervisorUnitPath(); err == nil {
			return p, "systemd"
		}
	}
	return "", ""
}

// detectDaemonSupervisor reports the supervisor provisioned FOR townRoot, or
// nil when the daemon is meant to be run by hand (no plist / unit file
// installed) or the installed one belongs to another workspace: the file is
// per user, and its WorkingDirectory is the town it was provisioned for —
// kickstarting it from a different town would restart that town's daemon
// and then wait for this town's lock in vain. A file that exists but cannot
// be read is an error, never "no supervisor": the whole point of the check
// is to never hand-spawn beside a provisioned one.
func detectDaemonSupervisor(townRoot string) (*daemonSupervisor, error) {
	p, kind := supervisorFilePath()
	if kind == "" {
		// No resolvable home means nowhere a plist could have been
		// provisioned; same reading as templates.SupervisorStatus.
		return nil, nil
	}
	isFor, err := templates.SupervisorFileIsFor(p, townRoot)
	if err != nil || !isFor {
		return nil, err
	}
	switch kind {
	case "launchd":
		return &daemonSupervisor{
			name:      "launchd",
			start:     []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), templates.LaunchdLabel)},
			stop:      []string{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), templates.LaunchdLabel)},
			bootstrap: []string{"launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), p},
			load:      fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), p),
		}, nil
	case "systemd":
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

// syncSupervisorFile brings an installed supervisor file up to date with the
// binary now running, and reports whether it had to rewrite it (gt-x872).
//
// A supervisor file is derived state, and one part of it is compiled into the
// binary that wrote it: launchd's ExitTimeOut, from daemon.ShutdownBudget. Only
// a provision writes one and a provision only runs on request, so a town
// provisioned before ShutdownBudget grew keeps the old value for as long as
// nobody rewrites the file — and launchd SIGKILLs the daemon part-way through
// its shutdown on every restart in the meantime, which is the failure the key
// exists to prevent. What is repairable, and what is deliberately left alone,
// is templates.SupervisorFileRepair's call; this is the installation half.
//
// A repair that cannot be made is warned about and then dropped: a job file
// that is behind is a reason to restart the daemon under a worse ExitTimeOut,
// never a reason to refuse to start it at all.
//
// A rewrite only changes the file. Making the service manager act on it is
// separate and deliberate: both managers cache a job definition at load time,
// so a changed file reaches the running job only through the unload and load
// that startFromFile does, never through an in-place restart.
func syncSupervisorFile(townRoot string) (rewritten bool) {
	path, kind := supervisorFilePath()
	if kind == "" {
		return false
	}
	content, repair, err := templates.SupervisorFileRepair(path, kind, townRoot, daemon.ShutdownBudget)
	if err == nil && repair {
		err = installSupervisorFile(path, content)
	}
	if err != nil {
		style.PrintWarning("could not bring the %s job file up to date: %v", kind, err)
		return false
	}
	return repair
}

// installSupervisorFile writes content over the supervisor file at path,
// whole and moved into place: a truncate-and-write leaves a half-written plist
// behind if this process dies mid-write, and the next bootstrap would then
// fail on it — no daemon, and nothing to say why. Same directory, so the
// rename is atomic.
func installSupervisorFile(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".com.gastown.daemon.*")
	if err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeded
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	return nil
}

// reconcileSupervisorJob brings the supervisor job for townRoot in line with
// the binary now running, for callers that found the daemon already up:
// startDaemon and restartDaemon do the same reconciliation on their own paths,
// so this is the town-boot version of it. The returned note is "" when there
// was nothing to do — no supervisor, no file, or a current one.
//
// It is a restart, not just a file write: a service manager caches a job
// definition when it loads it, so a rewritten file does not reach the running
// job by itself (gt-x872). That is an interruption the caller did not ask for
// — bounded, and paid once per change of the file's derived values, since the
// rewrite is what makes the next boot find the file current. The alternative
// is an ExitTimeOut that never takes effect on any town that is not
// re-provisioned, which is the whole of the bug.
func reconcileSupervisorJob(townRoot string, daemonPID int) (note string, err error) {
	sup, err := detectDaemonSupervisor(townRoot)
	if err != nil || sup == nil {
		return "", err
	}
	if !syncSupervisorFile(townRoot) {
		return "", nil
	}

	if err := sup.startFromFile(); err != nil {
		return "", fmt.Errorf("reloading the %s job from its rewritten file: %w", sup.name, err)
	}
	// The job is back but the daemon it starts needs a moment to take the
	// lock, and the caller reads that lock for its own status line. Unlike the
	// restart paths, the PID that must be replaced is the one the caller
	// already read: a wait that stopped at "the lock is held" would report the
	// daemon on its way out and call this a success.
	if _, err := waitForRestart(townRoot, daemonPID); err != nil {
		return "", fmt.Errorf("daemon did not come back after the %s job was reloaded from its file: %w", sup.name, err)
	}
	return fmt.Sprintf("%s job reloaded from an updated file", sup.name), nil
}

// startFromFile (re)starts the daemon from the job's file on disk, for the
// case where that file was just rewritten: a service manager reads a job
// definition when it loads it and caches it from then on, so the only way a
// rewritten file reaches the job is to unload it and load it again. That is
// what this does — sup.stop, then sup.bootstrap.
//
// It is deliberately not how a restart normally goes. Unloading leaves the
// daemon unsupervised between the two commands, the window restartDaemon
// exists to avoid (gt-sq9e), and spending it is worth it only when the job
// definition actually changed — at most once per binary upgrade.
func (s *daemonSupervisor) startFromFile() error {
	// Unload only a job the manager knows: bootout on a job that is not there
	// fails, and startFromFile is also reached from start, where "not loaded"
	// (the state `gt daemon stop` leaves) is ordinary.
	if st := supervisorStateFor(s.name); st.Err == nil && st.Loaded {
		if err := supervisorRun(s.stop); err != nil {
			return fmt.Errorf("unloading the %s job to load its rewritten file: %w", s.name, err)
		}
	}
	return supervisorRun(s.bootstrap)
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
		rewritten := syncSupervisorFile(townRoot)
		var runErr error
		if rewritten {
			// The file the job will start from is the one just rewritten, so
			// the job has to be loaded from it rather than restarted in place.
			runErr = sup.startFromFile()
		} else {
			runErr = supervisorRun(sup.start)
			if runErr != nil {
				// kickstart / restart reach only a job the service manager knows.
				// One that gt daemon stop unloaded is bootstrapped instead, so that
				// a stop-then-start pair leaves a supervised daemon rather than the
				// hand spawn that recreates the crash loop (gt-3jrm).
				if st := supervisorStateFor(sup.name); st.Err == nil && !st.Loaded {
					runErr = supervisorRun(sup.bootstrap)
				}
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

// handStopConfirmBudget bounds waitForDaemonGone: how long restartDaemon's
// hand path waits, after stopDaemonDirect returns, for the OS to actually
// finish tearing the old process down and releasing daemon.lock. SIGKILL
// itself is near-instant, so this is confirming the OS finished, not waiting
// out a graceful shutdown — a short budget, unlike restartWaitBudget above.
const handStopConfirmBudget = 5 * time.Second

// handStopConfirmAttempts is handStopConfirmBudget expressed as a poll count
// the same way restartWaitAttempts is (see its doc): against the fixed
// production poll interval, not the possibly-shrunk test one.
const handStopConfirmAttempts = int(handStopConfirmBudget / (100 * time.Millisecond))

// waitForDaemonGone polls the lock for up to handStopConfirmBudget and
// reports an error if the daemon is still running at the end of it.
func waitForDaemonGone(townRoot string) error {
	for range handStopConfirmAttempts {
		running, _, err := daemonIsRunning(townRoot)
		if err != nil {
			return fmt.Errorf("checking daemon status: %w", err)
		}
		if !running {
			return nil
		}
		time.Sleep(daemonPollInterval)
	}
	return fmt.Errorf("daemon still held its lock %s after stopping (check logs with 'gt daemon logs')", handStopConfirmBudget)
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
// The supervisor's file is reconciled first (syncSupervisorFile). A restart is
// both the act a stale ExitTimeOut endangers — it is what asks the service
// manager to stop the daemon — and the one gt-driven moment the file can be
// loaded afresh: when the reconciliation finds the file stale it rewrites it
// and restarts the job from it, so the new value is in force from this restart
// onward rather than at some later one.
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
		// stopDaemonDirect (daemon.StopDaemon) sends SIGKILL and returns
		// without confirming it took: its own caller (`gt daemon stop`) has
		// nothing further to do with the process either way. This caller
		// does — it is about to spawn a new daemon into the same lock — so
		// starting that race before the old holder is confirmed gone risks
		// the new process losing the flock to a not-quite-dead old one.
		if err := waitForDaemonGone(townRoot); err != nil {
			return "", 0, err
		}
		return startDaemon(townRoot)
	}
	var runErr error
	if syncSupervisorFile(townRoot) {
		// This is the restart the ExitTimeOut it carries exists for: the job
		// is reloaded from the file, so the new value is in force before the
		// next stop this restart causes — and for every one after it.
		runErr = sup.startFromFile()
	} else {
		runErr = supervisorRun(sup.start)
	}
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

// daemonStartupMargin is how long waitForRestart allows the INCOMING daemon
// to run its own preflight (Dolt metadata repair, opening beads stores) and
// acquire daemon.lock, on top of the time the OUTGOING one takes to let go of
// it. It has no measured source the way ShutdownBudget's steps do; it is a
// deliberately generous margin.
const daemonStartupMargin = 30 * time.Second

// restartWaitBudget bounds waitForRestart's poll. It is derived from the two
// real enforcement mechanisms that bound how long the OLD daemon can hold
// daemon.lock after a restart is asked for — not from Daemon.shutdown's own
// step budgets, which neither restart path actually lets run to completion:
//
//   - Under a provisioned launchd job (the only path that uses this budget;
//     see restartDaemon), `gt daemon restart` is `launchctl kickstart -k`,
//     which sends SIGTERM and — critically — launchd itself SIGKILLs the job
//     if it has not exited within ExitTimeOut. ProvisionSupervisor sets that
//     to daemon.ShutdownBudget (see internal/templates), so the old daemon is
//     guaranteed gone within ShutdownBudget of the restart, whatever its own
//     shutdown steps add up to.
//   - The hand path (no supervisor) does not use this function at all:
//     restartDaemon calls stopDaemonDirect (daemon.StopDaemon) instead, whose
//     own SIGKILL grace is constants.ShutdownNotifyDelay (500 ms) — an order
//     of magnitude tighter, because there is no supervisor to leave the job
//     loaded for, so the caller can afford to force the issue immediately.
//
// So ShutdownBudget is the bound this function actually has to plan around;
// daemonStartupMargin covers the incoming daemon on top of it.
const restartWaitBudget = daemon.ShutdownBudget + daemonStartupMargin

// restartWaitAttempts is how many times waitForRestart polls: chosen so that
// restartWaitAttempts * daemonPollInterval's PRODUCTION value (100ms) equals
// restartWaitBudget. It is computed against that fixed default, not against
// the current daemonPollInterval, so that shrinking daemonPollInterval in
// tests (see its doc) shrinks the real wait proportionally instead of
// canceling out against a count recomputed from the same shrunk value.
const restartWaitAttempts = int(restartWaitBudget / (100 * time.Millisecond))

// waitForRestart polls the lock for up to restartWaitBudget and returns the
// PID of a daemon other than oldPID. The longer budget than waitForDaemon's
// is for the restart's extra step: the outgoing process has to release the
// lock and the supervisor has to bring the job back before there is anything
// to see, and a wait that times out early would report a restart that did
// happen as a failure.
//
// pid <= 0 never satisfies the wait, even when running is true and 0 !=
// oldPID: IsRunning reports running with pid 0 when the lock is held but the
// PID file has not been written yet (the same race daemon.StopDaemon's own
// comment calls out) — a start still in flight, not the new daemon this is
// waiting for.
func waitForRestart(townRoot string, oldPID int) (int, error) {
	for range restartWaitAttempts {
		time.Sleep(daemonPollInterval)
		running, pid, err := daemonIsRunning(townRoot)
		if err != nil {
			return 0, fmt.Errorf("checking daemon status: %w", err)
		}
		if running && pid > 0 && pid != oldPID {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon failed to restart (check logs with 'gt daemon logs')")
}
