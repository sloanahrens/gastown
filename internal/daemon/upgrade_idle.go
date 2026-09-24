package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/version"
)

// isIdleForUpgrade reports whether restarting the daemon now would kill no
// in-flight work: no script plugin, compactor, boot triage, scheduled
// slings, mayor dispatch or patrol watchdog run, and no main_branch_test
// past its slot wait. pourDoctorMolecule and the Dolt goroutines are not
// counted: they are short or restartable.
func (d *Daemon) isIdleForUpgrade() bool {
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
	return true
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
