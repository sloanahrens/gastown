package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/version"
)

// isIdleForUpgrade reports whether restarting the daemon now would kill no
// in-flight work: no script plugin, compactor, boot triage, scheduled
// slings or patrol watchdog run, no steward scan, plan scan or
// job, no landing-worker pass, no scheduled_maintenance gc cycle, and no
// install holding install-gt.lock. The Dolt goroutines are not counted: they
// are short or restartable. Post-landing runs and tier sweep cycles are not
// here either: checkUpgradeRestart holds a bounded wait for each instead
// (postLandRestartCap, tierSweepRestartCap), and a predicate that never
// cleared would hold a restart forever.
func (d *Daemon) isIdleForUpgrade() bool {
	if d.maintenanceGCRunning.Load() {
		return false
	}
	return d.daemonWorkIdle()
}

// daemonWorkIdle is isIdleForUpgrade without the gc cycle's own flag, so the
// gc cycle's quiet-window guard (maintenanceQuiet) can reuse the predicate
// without reading itself as busy.
func (d *Daemon) daemonWorkIdle() bool {
	if d.scripts.runningCount() > 0 {
		return false
	}
	d.compactorDogMu.Lock()
	compactor := d.compactorDogRunning
	d.compactorDogMu.Unlock()
	if compactor {
		return false
	}
	if d.scheduledSlingsRunning.Load() ||
		d.specDispatchRunning.Load() || d.patrolScanRunning.Load() ||
		d.landingPasses.Load() > 0 || d.stewardRunning.Load() || d.stewardPlanScan.Load() {
		return false
	}
	if d.stewardJobsRunning() {
		return false
	}
	// Checked last: it is the only check that touches the filesystem.
	if d.installLockHeld() {
		return false
	}
	return true
}

// stewardJobsRunning reports whether a steward job is in flight. A restart
// kills the job, and while that no longer spends the event's retry, the agent
// has done a session's work for nothing: at merge-cadence upgrades a job
// would rarely live long enough to finish (gt-9bioi.5).
func (d *Daemon) stewardJobsRunning() bool {
	d.stewardRunnerMu.Lock()
	runner := d.stewardRunner
	d.stewardRunnerMu.Unlock()
	return runner != nil && len(runner.Running()) > 0
}

// installLockPath is scripts/install-gt.sh's lock:
// ${INSTALL_GT_DAEMON_DIR:-$TOWN_ROOT/daemon}/install-gt.lock. The daemon
// never sets INSTALL_GT_DAEMON_DIR, so its town's daemon dir is the one.
func installLockPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "install-gt.lock")
}

// installLockHeld reports whether an install-gt.sh run holds its lock. An
// install replaces the binary before its smoke check; restarting inside that
// window would exec an unverified binary that a failed smoke check then rolls
// back underneath the daemon. The probe never blocks: it opens the existing
// file (never creating it), tries a non-blocking exclusive flock and releases
// at once. A missing file means no install has ever run: not held. Any other
// error is treated as held, the safe side (restart-pending-stuck escalates if
// it persists).
func (d *Daemon) installLockHeld() bool {
	path := installLockPath(d.config.TownRoot)
	// SetFlag replaces flock's default O_CREATE|O_RDONLY: never create it.
	lock := flock.New(path, flock.SetFlag(os.O_RDONLY))
	locked, err := lock.TryLock()
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		d.logger.Printf("upgrade: cannot probe %s: %v; treating the install lock as held", path, err)
		return true
	}
	if !locked {
		return true
	}
	_ = lock.Unlock()
	return false
}

// buildCommit is the daemon's own build commit: buildCommitFn's when a test
// set one, else the version package's.
func (d *Daemon) buildCommit() string {
	if d.buildCommitFn != nil {
		return d.buildCommitFn()
	}
	return version.BuildCommit()
}

// resolveOwnCommit returns the full SHA of this build when the gastown source
// repo can resolve it, else the (short) build commit.
func (d *Daemon) resolveOwnCommit() string {
	own := d.buildCommit()
	if own == "" {
		return ""
	}
	repo := filepath.Join(d.config.TownRoot, "gastown", "mayor", "rig")
	full, err := d.gitAt(repo).Rev(own + "^{commit}")
	if err != nil {
		return own
	}
	return strings.TrimSpace(full)
}
