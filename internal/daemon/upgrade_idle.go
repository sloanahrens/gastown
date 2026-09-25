package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/version"
)

// isIdleForUpgrade reports whether restarting the daemon now would kill no
// in-flight work: no script plugin, compactor, boot triage, scheduled
// slings, mayor dispatch or patrol watchdog run, no main_branch_test past its
// slot wait, no scheduled_maintenance gc cycle, and no install holding
// install-gt.lock. pourDoctorMolecule and the Dolt goroutines are not
// counted: they are short or restartable.
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
	if d.bootTriageInFlight.Load() || d.scheduledSlingsRunning.Load() ||
		d.mayorDispatchRunning.Load() || d.patrolWatchdogRunning.Load() {
		return false
	}
	if d.mainBranchTestRunning.Load() && !d.mainBranchTestWaitingSlot.Load() {
		return false
	}
	// Checked last: it is the only check that touches the filesystem.
	if d.installLockHeld() {
		return false
	}
	return true
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

// buildCommitFn is the daemon's own build commit; a test seam.
var buildCommitFn = version.BuildCommit

// resolveOwnCommit returns the full SHA of this build when the gastown source
// repo can resolve it, else the (short) build commit.
func (d *Daemon) resolveOwnCommit() string {
	own := buildCommitFn()
	if own == "" {
		return ""
	}
	repo := filepath.Join(d.config.TownRoot, "gastown", "mayor", "rig")
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", own+"^{commit}").Output()
	if err != nil {
		return own
	}
	return strings.TrimSpace(string(out))
}
