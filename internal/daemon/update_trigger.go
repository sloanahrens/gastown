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
)

// updateTriggerState is the trigger's memory between heartbeats: when this
// daemon last launched the job, and the settings error it last logged.
type updateTriggerState struct {
	mu sync.Mutex
	// launchedAt is the clock reading of the last launch this daemon started.
	launchedAt time.Time
	// configErr is the last settings error printed, so an unchanged broken
	// config logs once rather than once per heartbeat.
	configErr string
}

// triggerUpdate is the heartbeat's update block: decide whether an update is
// due and, when it is, launch `gt update --from-daemon` without waiting for
// it. The daemon never runs the update itself; the job outlives the daemon
// because an install makes the daemon restart.
func (d *Daemon) triggerUpdate() {
	settings, ok := d.updateSettings()
	if !ok || !settings.Enabled {
		return
	}

	now := d.clk().Now()
	facts, err := d.updateFacts(now)
	if err != nil {
		// An unreadable fact is unknown, not absent: a launch would be a
		// decision made without it, so this heartbeat does not launch.
		d.logger.Printf("update: cannot decide whether an update is due: %v; not launching", err)
		return
	}

	due, reason := updateDue(now, settings, facts)
	if !due {
		return
	}
	d.logger.Printf("update: due (%s); launching gt update", reason)

	launch := d.updateLaunch
	if launch == nil {
		d.logger.Printf("update: no launcher is wired into this daemon; not launching")
		return
	}
	// A context of its own: the child must outlive the daemon's cancellation
	// when the install restarts it.
	if err := launch(context.Background(), reason); err != nil {
		d.logger.Printf("update: launch failed: %v", err)
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

// noteUpdateLaunch records the launch so the next heartbeat waits out
// updateLaunchSuppression before deciding again.
func (d *Daemon) noteUpdateLaunch(now time.Time) {
	d.updateTrigger.mu.Lock()
	d.updateTrigger.launchedAt = now
	d.updateTrigger.mu.Unlock()
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
		facts.LastStarted, facts.LastEnded = st.StartedAt, st.EndedAt
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
		// which is how rebuild_gt's own cycle reads the same town.
		return facts, nil
	}
	info, ok := d.rebuildGTStale(repoRoot)
	if !ok {
		return facts, errors.New("the installed build's staleness could not be read")
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

// updateLandingsWaiting counts the landings recorded on the rig whose checkout
// the binary is built from that the installed build does not contain: the ones
// waiting to deploy. The walk starts at the newest record and stops at the
// first landing the build contains, because every older one is in the build
// too.
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

// launchUpdate starts `gt update --from-daemon` as a detached child and
// returns at once. The child's output goes to a launch log under the update
// directory; the child is never waited on, so it survives this daemon being
// restarted by the install it performs.
func (d *Daemon) launchUpdate(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd, logFile, err := buildUpdateLaunch(d.config.TownRoot, d.clk().Now(), os.Executable)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	return cmd.Start()
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
