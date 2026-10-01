package daemon

import (
	"github.com/steveyegge/gastown/internal/session"
)

// daemonSeams holds the collaborators tests replace in the heartbeat, the
// upgrade restart and the legacy-socket cleanup. Each nil field is the
// production behavior; the methods below pick.
type daemonSeams struct {
	heartbeatWork        func(d *Daemon, s *State)
	upgradeEscalate      func(d *Daemon, key, msg string)
	cleanupLegacySockets func(townRoot string) (defaultCleaned, baseCleaned int)
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
// hold, returning how many sessions each cleanup removed. reg marks which
// sessions are Gas Town's.
func (s daemonSeams) cleanupLegacySocketsFor(reg *session.PrefixRegistry, townRoot string) (int, int) {
	if s.cleanupLegacySockets != nil {
		return s.cleanupLegacySockets(townRoot)
	}
	return cleanupLegacySockets(reg, townRoot)
}
