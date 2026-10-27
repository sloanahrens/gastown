package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmuxsweep"
)

// daemonSeams holds the collaborators tests replace in the heartbeat, the
// upgrade restart, the legacy-socket cleanup, the test-pollution sweep and
// the bd call census. Each nil field is the production behavior; the methods
// below pick.
type daemonSeams struct {
	heartbeatWork        func(d *Daemon, s *State, lifecycle bool)
	heartbeatStep        func(d *Daemon, step heartbeatStep)
	upgradeEscalate      func(d *Daemon, key, msg string)
	upgradeHealJob       func(townRoot string) (upgradeHealJob, bool)
	upgradeHealRun       func(argv []string) error
	cleanupLegacySockets func(townRoot string) (defaultCleaned, baseCleaned int)
	testSocketSweep      func() (tmuxsweep.Report, error)
	imposterSweep        func(townRoot string)
	orphanReap           func()
	censusTicker         func(interval time.Duration) (ticks <-chan time.Time, stop func())
	censusLog            func(ctx context.Context, every <-chan time.Time, logf func(string, ...any))
	// shadowEncodeJSON encodes one reviewed landing into the deep-review
	// shadow note's landings.jsonl line. A record's fields are strings and
	// timestamps, so the production encoder cannot fail; the seam exists so
	// the note's failure branch has a test.
	shadowEncodeJSON func(any) ([]byte, error)
}

// encodeShadowLanding renders one landing for the shadow note's JSON lines.
func (s daemonSeams) encodeShadowLanding(v any) ([]byte, error) {
	if s.shadowEncodeJSON != nil {
		return s.shadowEncodeJSON(v)
	}
	return json.Marshal(v)
}

// startCensusTicker returns the census's tick channel and the func that stops
// it.
func (s daemonSeams) startCensusTicker(interval time.Duration) (<-chan time.Time, func()) {
	if s.censusTicker != nil {
		return s.censusTicker(interval)
	}
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

// runCensusLog blocks while logging one census line per tick until ctx ends.
func (s daemonSeams) runCensusLog(ctx context.Context, every <-chan time.Time, logf func(string, ...any)) {
	if s.censusLog != nil {
		s.censusLog(ctx, every, logf)
		return
	}
	beads.StartCensusLog(ctx, every, logf)
}

// upgradeHealJobFor names the launchd job the upgrade heal helper arms under,
// or false when there is none for townRoot.
func (s daemonSeams) upgradeHealJobFor(townRoot string) (upgradeHealJob, bool) {
	if s.upgradeHealJob != nil {
		return s.upgradeHealJob(townRoot)
	}
	return upgradeHealSupervisorJob(townRoot)
}

// runUpgradeHeal runs one launchctl argv for the upgrade heal helper. Unit
// tests replace it: no test may load or unload a real launchd job.
func (s daemonSeams) runUpgradeHeal(argv []string) error {
	if s.upgradeHealRun != nil {
		return s.upgradeHealRun(argv)
	}
	return runUpgradeHealCommand(upgradeHealCommandTimeout, argv)
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
