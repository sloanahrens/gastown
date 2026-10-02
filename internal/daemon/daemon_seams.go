package daemon

import (
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmuxsweep"
)

// daemonSeams holds the collaborators tests replace in the heartbeat, the
// upgrade restart, the legacy-socket cleanup and the test-pollution sweep.
// Each nil field is the production behavior; the methods below pick.
type daemonSeams struct {
	heartbeatWork        func(d *Daemon, s *State, lifecycle bool)
	heartbeatStep        func(d *Daemon, step heartbeatStep)
	upgradeEscalate      func(d *Daemon, key, msg string)
	cleanupLegacySockets func(townRoot string) (defaultCleaned, baseCleaned int)
	testSocketSweep      func() (tmuxsweep.Report, error)
	imposterSweep        func(townRoot string)
	orphanReap           func()
}

// runHeartbeatWork runs the body of one heartbeat; lifecycle false holds
// the dispatch, kill and restart steps.
func (s daemonSeams) runHeartbeatWork(d *Daemon, state *State, lifecycle bool) {
	if s.heartbeatWork != nil {
		s.heartbeatWork(d, state, lifecycle)
		return
	}
	d.heartbeatWork(state, lifecycle)
}

// runHeartbeatStep runs one step of a heartbeat.
func (s daemonSeams) runHeartbeatStep(d *Daemon, step heartbeatStep) {
	if s.heartbeatStep != nil {
		s.heartbeatStep(d, step)
		return
	}
	step.run(d)
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
// hold, returning how many sessions each cleanup removed. reg marks which
// sessions are Gas Town's.
func (s daemonSeams) cleanupLegacySocketsFor(reg *session.PrefixRegistry, townRoot string) (int, int) {
	if s.cleanupLegacySockets != nil {
		return s.cleanupLegacySockets(townRoot)
	}
	return cleanupLegacySockets(reg, townRoot)
}

// sweepTestSockets scans the machine's tmux socket directory for abandoned
// test servers and reaps them. Unit tests replace it: no test may touch the
// real socket directory.
func (s daemonSeams) sweepTestSockets() (tmuxsweep.Report, error) {
	if s.testSocketSweep != nil {
		return s.testSocketSweep()
	}
	return sweepTestSockets()
}
