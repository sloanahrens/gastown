package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
)

const (
	// updateHeartbeatFresh is how recent a running run's heartbeat must be for
	// the trigger to read the run as in progress. The job refreshes it every 15
	// seconds, so three minutes of silence is a dead run, not a slow step.
	updateHeartbeatFresh = 3 * time.Minute

	// updateLaunchSuppression is how long after a launch the trigger stays
	// quiet: the job takes a few seconds to write its first state, and a
	// heartbeat inside that gap must not start a second run.
	updateLaunchSuppression = 5 * time.Minute

	// updateLandingRecords bounds the landing records read to decide whether
	// anything waits to deploy. It is the window the attention queue reads;
	// landings older than it are long since in the installed build.
	updateLandingRecords = 50

	// updateRemoteTimeout bounds the ls-remote the failed-run hold makes for
	// main's tip. The recovery heartbeat defaults to 3 minutes and the
	// recovery_heartbeat_interval setting can change it, so the bound sits well
	// inside one beat at the default; a read that outlasts it is an unknown tip.
	updateRemoteTimeout = 30 * time.Second
)

// updateTriggerState is the trigger's state between heartbeats: the launch
// seam, when this daemon last launched the job, when a launch was last
// refused, and the settings error it last logged.
type updateTriggerState struct {
	mu sync.Mutex
	// launch starts the update job from the heartbeat's update block. New
	// wires it to (*Daemon).launchUpdate, the detached `gt update --from-daemon`
	// child; a test replaces it to watch the decision without starting a
	// process. Nil (a Daemon built without New) refuses to launch rather than
	// start one implicitly. Set before the heartbeat runs, never after.
	launch func(ctx context.Context, reason string) error
	// launchedAt is the clock reading of the last launch this daemon started.
	launchedAt time.Time
	// refusedAt is the clock reading of the last launch the launcher refused.
	// A refused launch waits out updateLaunchSuppression, as a launched one
	// does, so a broken launcher is not retried on every heartbeat.
	refusedAt time.Time
	// configErr is the last settings error printed, so an unchanged broken
	// config logs once rather than once per heartbeat.
	configErr string
	// notDueReason is the last not-due reason printed, so a hold that stands
	// logs once rather than once per heartbeat.
	notDueReason string
	// remoteErr is the last main-tip read error printed, so an origin that
	// stays unreachable logs once rather than once per heartbeat.
	remoteErr string
}

// triggerUpdate is the heartbeat's update block: decide whether an update is
// due and, when it is, launch `gt update --from-daemon` without waiting for
// it. The daemon never runs the update itself; the job outlives the daemon
// because an install makes the daemon restart. The job's alerts run first,
// before the enabled check: a hand-started run can die with the feature off.
func (d *Daemon) triggerUpdate() {
	now := d.clk().Now()
	d.checkUpdateAlerts(now)
	settings, ok := d.updateSettings()
	if !ok || !settings.Enabled {
		return
	}

	facts, err := d.updateFacts(now)
	if err != nil {
		// An unreadable fact is unknown, not absent: a launch would be a
		// decision made without it, so this heartbeat does not launch.
		d.logger.Printf("update: cannot decide whether an update is due: %v; not launching", err)
		return
	}

	due, reason := updateDue(now, settings, facts)
	if !due {
		// Name the hold, so an operator can see why updates have stopped
		// rather than only when one starts.
		d.noteUpdateNotDue(reason)
		return
	}
	d.clearUpdateNotDue()
	if d.updateRefusedRecently(now) {
		return
	}
	d.logger.Printf("update: due (%s); launching gt update", reason)

	launch := d.updateTrigger.launch
	if launch == nil {
		d.logger.Printf("update: no launcher is wired into this daemon; not launching")
		return
	}
	// A context of its own: the child must outlive the daemon's cancellation
	// when the install restarts it.
	if err := launch(context.Background(), reason); err != nil {
		d.logger.Printf("update: launch failed: %v; not retrying for %s", err, updateLaunchSuppression)
		d.noteUpdateRefusal(now)
		return
	}
	d.noteUpdateLaunch(now)
}

// updateSettings parses the daemon.patrols.update block. An absent block gives
// the defaults, which leave the trigger off. A block that does not parse is
// logged once per distinct message and read as off, so the heartbeat never
// launches on settings it could not understand.
func (d *Daemon) updateSettings() (config.UpdateSettings, bool) {
	var updateCfg *config.UpdateConfig
	if d.patrolConfig != nil && d.patrolConfig.Patrols != nil {
		updateCfg = d.patrolConfig.Patrols.Update
	}
	settings, err := updateCfg.Settings()
	if err != nil {
		d.noteUpdateConfigError(err.Error())
		return config.UpdateSettings{}, false
	}
	d.clearUpdateConfigError()
	return settings, true
}

// noteUpdateConfigError logs a settings error once per distinct message.
func (d *Daemon) noteUpdateConfigError(msg string) {
	d.updateTrigger.mu.Lock()
	seen := d.updateTrigger.configErr == msg
	if !seen {
		d.updateTrigger.configErr = msg
	}
	d.updateTrigger.mu.Unlock()
	if !seen {
		d.logger.Printf("update: config error: %s; the trigger stays off until it parses", msg)
	}
}

// clearUpdateConfigError forgets the last settings error once the block parses
// again, so a later recurrence is reported.
func (d *Daemon) clearUpdateConfigError() {
	d.updateTrigger.mu.Lock()
	d.updateTrigger.configErr = ""
	d.updateTrigger.mu.Unlock()
}

// noteUpdateNotDue logs a not-due reason once per distinct value, so a hold
// that stands is visible without one line per heartbeat.
func (d *Daemon) noteUpdateNotDue(reason string) {
	d.updateTrigger.mu.Lock()
	seen := d.updateTrigger.notDueReason == reason
	if !seen {
		d.updateTrigger.notDueReason = reason
	}
	d.updateTrigger.mu.Unlock()
	if !seen {
		d.logger.Printf("update: not due — %s", reason)
	}
}

// clearUpdateNotDue forgets the last not-due reason once a run is due, so a
// later hold on the same reason is reported again.
func (d *Daemon) clearUpdateNotDue() {
	d.updateTrigger.mu.Lock()
	d.updateTrigger.notDueReason = ""
	d.updateTrigger.mu.Unlock()
}

// noteUpdateLaunch records the launch so the next heartbeat waits out
// updateLaunchSuppression before deciding again.
func (d *Daemon) noteUpdateLaunch(now time.Time) {
	d.updateTrigger.mu.Lock()
	d.updateTrigger.launchedAt = now
	d.updateTrigger.mu.Unlock()
}

// noteUpdateRefusal records a launch the launcher refused, so the next
// heartbeats wait out updateLaunchSuppression before trying again.
func (d *Daemon) noteUpdateRefusal(now time.Time) {
	d.updateTrigger.mu.Lock()
	d.updateTrigger.refusedAt = now
	d.updateTrigger.mu.Unlock()
}

// updateRefusedRecently reports whether the launcher refused a launch within
// updateLaunchSuppression of now.
func (d *Daemon) updateRefusedRecently(now time.Time) bool {
	d.updateTrigger.mu.Lock()
	defer d.updateTrigger.mu.Unlock()
	refused := d.updateTrigger.refusedAt
	return !refused.IsZero() && now.Sub(refused) < updateLaunchSuppression
}

// lastUpdateLaunch is when this daemon last started the job, zero when it has
// not.
func (d *Daemon) lastUpdateLaunch() time.Time {
	d.updateTrigger.mu.Lock()
	defer d.updateTrigger.mu.Unlock()
	return d.updateTrigger.launchedAt
}

// updateFacts gathers what the due check knows, from the same sources the
// rebuild_gt cycle reads: the update job's state file, the pending-install
// marker, the installed build's staleness against main, and the landing
// records of the rig whose checkout the binary is built from. An unreadable
// source is an error, never an empty value.
func (d *Daemon) updateFacts(now time.Time) (UpdateFacts, error) {
	facts := UpdateFacts{Standby: d.standbyReason(now)}

	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	st, err := store.Read()
	switch {
	case err == nil:
		if !knownUpdateOutcome(st.Outcome) {
			// An outcome no run writes is unreadable: read as ok it would launch a run.
			return facts, fmt.Errorf("the update state's outcome %q is not a run outcome", st.Outcome)
		}
		facts.LastStarted, facts.LastEnded = st.StartedAt, st.EndedAt
		facts.LastOutcome, facts.LastTarget = st.Outcome, st.Target.SHA
		if st.Outcome == update.OutcomeRunning &&
			!st.HeartbeatAt.IsZero() && now.Sub(st.HeartbeatAt) < updateHeartbeatFresh {
			facts.Running = true
		}
	case errors.Is(err, update.ErrNoState):
		// No run has written state; both times stay zero.
	default:
		return facts, err
	}

	// A launch inside the suppression window is a run the state file does not
	// show yet. The clock is the one this daemon recorded, not the file's.
	if launched := d.lastUpdateLaunch(); !launched.IsZero() && now.Sub(launched) < updateLaunchSuppression {
		facts.Running = true
	}

	pending, err := d.rebuildGTPendingStore().Pending(now)
	if err != nil {
		return facts, fmt.Errorf("read the pending-install marker: %w", err)
	}
	facts.InstallPending = pending != nil

	repoRoot, err := version.GetRepoRootForTown(d.config.TownRoot)
	if err != nil {
		// No gt source checkout: there is no binary for an install to move,
		// which is how rebuild_gt's own cycle reads the same town. Main's sha
		// cannot be read either, so record the missing checkout rather than
		// leave MainSHA empty for the failed-run hold to read as unknown
		// (gt-fj9n1).
		facts.SourceMissing = true
		return facts, nil
	}
	info, ok := d.rebuildGTStale(repoRoot)
	if !ok {
		return facts, errors.New("the installed build's staleness could not be read")
	}
	facts.MainSHA = strings.TrimSpace(info.RepoCommit)
	if facts.LastOutcome == update.OutcomeFailed || facts.LastOutcome == update.OutcomeAborted {
		// The checkout's own origin/main ref lags main until something fetches,
		// and a run that fails before the installer never fetches. The hold
		// compares the failed run's target with the tip the run itself reads, so
		// the same failed run cannot restart (gt-tb5vx).
		facts.MainSHA = d.updateRemoteMainSHA(repoRoot)
	}
	facts.MainMoved = info.IsStale
	if !info.IsStale {
		return facts, nil
	}

	waiting, err := d.updateLandingsWaiting(repoRoot, info.BinaryCommit)
	if err != nil {
		return facts, err
	}
	facts.Waiting = waiting
	return facts, nil
}

// updateRemoteMainSHA reads main's tip from origin the way the run does, bounded
// by updateRemoteTimeout. A read that fails returns empty, which the hold reads
// as an unknown main: it holds and names the by-hand command rather than
// comparing against a ref it cannot trust. Each read runs on the heartbeat, so
// an unreachable origin can hold a beat for up to updateRemoteTimeout. The error
// is logged once per distinct message, so the operator sees why the tip is
// unknown without a line on every beat.
func (d *Daemon) updateRemoteMainSHA(repoRoot string) string {
	run := d.updateRemoteRun
	if run == nil {
		run = update.ExecRun
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateRemoteTimeout)
	defer cancel()
	sha, err := update.RemoteMainSHA(ctx, run, repoRoot)
	if err != nil {
		d.noteRemoteMainError(err)
		return ""
	}
	d.clearRemoteMainError()
	return sha
}

// noteRemoteMainError logs a main-tip read error once per distinct message.
func (d *Daemon) noteRemoteMainError(err error) {
	msg := err.Error()
	d.updateTrigger.mu.Lock()
	seen := d.updateTrigger.remoteErr == msg
	if !seen {
		d.updateTrigger.remoteErr = msg
	}
	d.updateTrigger.mu.Unlock()
	if !seen {
		d.logger.Printf("update: cannot read main's tip from origin: %s; the failed-run hold reads main as unknown", msg)
	}
}

// clearRemoteMainError forgets the last main-tip read error once a read
// succeeds, so a later outage is reported again.
func (d *Daemon) clearRemoteMainError() {
	d.updateTrigger.mu.Lock()
	d.updateTrigger.remoteErr = ""
	d.updateTrigger.mu.Unlock()
}

// knownUpdateOutcome reports whether o is one a run writes to its state file.
func knownUpdateOutcome(o update.Outcome) bool {
	switch o {
	case update.OutcomeRunning, update.OutcomeOK, update.OutcomePartial,
		update.OutcomeAborted, update.OutcomeFailed, update.OutcomeInterrupted:
		return true
	}
	return false
}

// updateLandingsWaiting counts the landings recorded on the rig whose checkout
// the binary is built from that the installed build does not contain: the ones
// waiting to deploy. The walk starts at the newest record and stops at the
// first landing the build contains, because every older one is in the build
// too. A landing the checkout has not fetched yet is waiting as well: the build
// is made from the checkout, so it cannot contain a commit the checkout lacks.
func (d *Daemon) updateLandingsWaiting(repoRoot, binaryCommit string) (int, error) {
	if strings.TrimSpace(binaryCommit) == "" {
		return 0, errors.New("the installed build's commit is unknown, so no landing can be shown to be deployed")
	}
	file, err := land.RigLandingsFile(d.config.TownRoot, d.updateSourceRig(repoRoot))
	if err != nil {
		return 0, err
	}
	recs, err := file.Recent(updateLandingRecords)
	if err != nil {
		return 0, err
	}

	repo := d.gitAt(repoRoot)
	waiting := 0
	for i := len(recs) - 1; i >= 0; i-- {
		sha := strings.TrimSpace(recs[i].LandedCommit)
		if sha == "" {
			continue
		}
		// Only an install fetches the checkout, so a landing made after the last
		// fetch is a commit it does not know. The installed build is built from
		// the checkout, so that landing cannot be in it: it is waiting to deploy.
		known, err := repo.HasObject(sha)
		if err != nil {
			return 0, fmt.Errorf("read whether landing %s is known to the checkout: %w", sha, err)
		}
		if !known {
			waiting++
			continue
		}
		deployed, err := repo.IsAncestor(sha, binaryCommit)
		if err != nil {
			return 0, fmt.Errorf("read whether landing %s is in the installed build: %w", sha, err)
		}
		if deployed {
			break
		}
		waiting++
	}
	return waiting, nil
}

// updateSourceRig is the rig directory holding repoRoot, empty when it is not
// a rig directly under the town.
func (d *Daemon) updateSourceRig(repoRoot string) string {
	rel, err := filepath.Rel(filepath.Clean(d.config.TownRoot), filepath.Clean(repoRoot))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		rel = rel[:i]
	}
	return rel
}

// updateProcessStarter starts a prepared update command and hands back the
// wait that reaps it. Production is startUpdateProcess; a test injects its own
// so it can prove the wait runs off the heartbeat without a child process.
type updateProcessStarter func(cmd *exec.Cmd) (wait func(), err error)

// startUpdateProcess starts cmd and returns the wait that reaps it. The exit
// status is the job's own record (its state file and history), so the wait
// only reaps.
func startUpdateProcess(cmd *exec.Cmd) (func(), error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() { _ = cmd.Wait() }, nil
}

// launchUpdate starts `gt update --from-daemon` as a detached child and
// returns at once. The child's output goes to a launch log under the update
// directory. The child is waited on from a goroutine of its own, so an exited
// job is reaped rather than left a zombie, and the wait never holds the
// heartbeat; Setsid already detached it, so an install restarting this daemon
// does not take the job down with the daemon's session.
func (d *Daemon) launchUpdate(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd, logFile, err := buildUpdateLaunch(d.config.TownRoot, d.clk().Now(), os.Executable)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	starter := d.updateStarter
	if starter == nil {
		starter = startUpdateProcess
	}
	wait, err := starter(cmd)
	if err != nil {
		return err
	}
	go wait()
	return nil
}

// buildUpdateLaunch builds, but does not start, the detached update command:
// executable's binary with `update --from-daemon`, its output going to a new
// launch log under the update directory. The caller closes the log file after
// Start; the child holds its own copy of the descriptor. Production passes
// os.Executable; a test passes its own to drive the failure path.
//
// Setsid puts the child in a session of its own, so an install restarting the
// daemon does not take the job down with the daemon's session and process
// group. No daemon helper sets it (util.SetDetachedProcessGroup sets Setpgid);
// the job needs the stronger detachment.
func buildUpdateLaunch(townRoot string, now time.Time, executable func() (string, error)) (*exec.Cmd, *os.File, error) {
	exe, err := executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locate the daemon executable: %w", err)
	}
	dir := update.Dir(townRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create the update directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, "launch-"+now.UTC().Format("20060102T150405Z")+".log")
	logFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open the launch log %s: %w", path, err)
	}
	cmd := exec.Command(exe, "update", "--from-daemon") //nolint:gosec // G204: this binary, fixed arguments
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, logFile, nil
}
