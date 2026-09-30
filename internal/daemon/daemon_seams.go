package daemon

import (
	"context"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/slot"
)

// daemonSeams holds the collaborators tests replace in the heartbeat, the
// upgrade restart, the legacy-socket cleanup and the main-branch check. Each
// nil field is the production behavior; the methods below pick.
type daemonSeams struct {
	heartbeatWork        func(d *Daemon, s *State)
	upgradeEscalate      func(d *Daemon, key, msg string)
	cleanupLegacySockets func(townRoot string) (defaultCleaned, baseCleaned int)

	// Main-branch check: the pool read behind the gate-busy skip, one rig's
	// run, one gate, and the starved-rig escalation.
	gatePoolStatus     func(townRoot string) (slot.Report, error)
	testRig            func(d *Daemon, rigName, rigPath string, timeout time.Duration) error
	gate               func(d *Daemon, ctx context.Context, rigName, commit, workDir, label, command string) error
	mainBranchEscalate func(d *Daemon, key, source, message string)

	// slots is the container gate the main-branch check takes its slot
	// through; nil is package slot's default gate, which asks docker.
	slots *slot.Gate
}

// runHeartbeatWork runs the body of one heartbeat.
func (s daemonSeams) runHeartbeatWork(d *Daemon, state *State) {
	if s.heartbeatWork != nil {
		s.heartbeatWork(d, state)
		return
	}
	d.heartbeatWork(state)
}

// escalateUpgrade raises an upgrade-restart alert. The real escalation
// retries for minutes under load, so it runs off the heartbeat.
func (s daemonSeams) escalateUpgrade(d *Daemon, key, msg string) {
	if s.upgradeEscalate != nil {
		s.upgradeEscalate(d, key, msg)
		return
	}
	go d.escalateAlert(key, "upgrade-restart", msg)
}

// cleanupLegacySocketsFor removes the legacy tmux sockets a town may still
// hold, returning how many sessions each cleanup removed.
func (s daemonSeams) cleanupLegacySocketsFor(townRoot string) (int, int) {
	if s.cleanupLegacySockets != nil {
		return s.cleanupLegacySockets(townRoot)
	}
	return cleanupLegacySockets(townRoot)
}

// mainBranchGatePoolStatus reads the container-gate pool's held/owner picture
// for the skip decision, so a test can pin a pool state — "gastown/refinery
// holds slot 0" — without racing a real refinery into a real flock.
//
// slot.StatusPoolLocksOnly, not slot.StatusPool: the decision reads nothing
// but held/owner, and its doc comment names exactly this caller shape as the
// one that should not pay for the `docker ps` cross-check (gt-a8kx). The
// container half answers "is an unwrapped suite running", which is the
// question AcquirePoolReal already asks below — and answers on the state it
// actually takes the slot in, rather than on a stale check from before the
// setup commands ran.
func (s daemonSeams) mainBranchGatePoolStatus(townRoot string) (slot.Report, error) {
	if s.gatePoolStatus != nil {
		return s.gatePoolStatus(townRoot)
	}
	cg := agentconfig.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	pool := slot.PoolFromConfig(cg)
	return slot.StatusPoolLocksOnly(townRoot, pool)
}

// mainBranchTestRig runs one rig's main-branch check for a cycle. A test hands
// the loop a pass, a failure, or an interruption directly: the alert
// arithmetic is decided over the verdicts, and reaching each shape through
// the real path would mean a bare repo, a gate config, and a real mid-run
// context cancellation for the interrupted one.
func (s daemonSeams) mainBranchTestRig(d *Daemon, rigName, rigPath string, timeout time.Duration) error {
	if s.testRig != nil {
		return s.testRig(d, rigName, rigPath, timeout)
	}
	return d.testRigMainBranch(rigName, rigPath, timeout)
}

// mainBranchGate runs one gate of a rig's main-branch check. A test hands the
// loop a verdict per gate: gates are walked in map order, so aiming a real
// mid-run cancellation at the gate a mixed-cycle test needs is not something a
// test can set up without racing the iteration.
func (s daemonSeams) mainBranchGate(d *Daemon, ctx context.Context, rigName, commit, workDir, label, command string) error {
	if s.gate != nil {
		return s.gate(d, ctx, rigName, commit, workDir, label, command)
	}
	return d.runCommandOnWorktree(ctx, rigName, commit, workDir, label, command)
}

// escalateMainBranch reports a rig starved off its baseline test by a
// persistently busy gate: the escalation is the only signal that a yielding
// patrol has stopped testing a rig at all, so it needs a test that drives it.
func (s daemonSeams) escalateMainBranch(d *Daemon, key, source, message string) {
	if s.mainBranchEscalate != nil {
		s.mainBranchEscalate(d, key, source, message)
		return
	}
	d.escalateAlert(key, source, message)
}
