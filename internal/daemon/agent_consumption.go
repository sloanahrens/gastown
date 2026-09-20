package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
)

// Input-consumption liveness for witness and refinery sessions (gt-eigw).
//
// The daemon's liveness check for these two roles is "does the tmux session
// exist and is the agent process alive". Both questions answered yes on
// 2026-09-09, when be-refinery sat at the prompt for ~15 minutes with wait-idle
// nudges stranded in Claude Code's input queue: nothing hooked, no turn started
// for an Enter keystroke, an immediate nudge or mail, and three MRs — one of
// them P0 — unclaimed behind it while the heartbeat logged
//
//	Refinery for beads already running, skipping spawn
//
// every four minutes. A live process that consumes no input is the one failure
// mode that check cannot see, so it is checked here instead.
//
// The probe itself is the already-reviewed one: tmux.DetectComposerStall
// (gt-hkhu), which requires BOTH unsubmitted input in the pane AND zero pane
// output for the whole frozen window. That conjunction is what keeps it off a
// healthy idle-await agent carrying a legitimately queued nudge.
//
// The action is deliberately conservative, and this is where it differs from
// the first attempt at gt-eigw, which was blocked for shipping a path that
// killed every healthy session ~5 minutes after startup:
//
//   - It never kills. The daemon does not own restart authority for a session
//     that is alive; the operator and the patrol do (see
//     witness.DetectStalledRefinery, whose contract is the same).
//   - It flushes the pending input with the keystroke an operator used to
//     unblock both 2026-09-18 refinery stalls, then re-probes.
//   - If the session is still holding the input after the flush, it escalates
//     ONCE per session per interval, naming the recovery command.
//
// A false positive is therefore bounded to "we submitted a queued nudge a few
// minutes earlier than it would have gone anyway, and sent one mail".

// consumptionRecheckDelay is the settle time allowed for the pane to repaint
// after a flush before the session is re-probed. Claude Code needs a moment to
// consume queued messages, and judging on the first frame would report a
// recovered session as still wedged. Var so tests can zero it.
var consumptionRecheckDelay = 750 * time.Millisecond

// consumptionEscalationInterval is how long the daemon stays quiet about the
// same still-wedged session after escalating once. Without it, a session that
// nobody restarts would escalate on every heartbeat.
const consumptionEscalationInterval = 30 * time.Minute

// consumptionEscalator rate-limits the escalation to one per session per
// interval. It is safe for concurrent use: ensureRefineriesRunning dispatches
// rigs in parallel.
type consumptionEscalator struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// shouldEscalate reports whether this session's wedge should be escalated now,
// recording the time when it returns true.
func (e *consumptionEscalator) shouldEscalate(session string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.last == nil {
		e.last = make(map[string]time.Time)
	}
	if last, ok := e.last[session]; ok && now.Sub(last) < consumptionEscalationInterval {
		return false
	}
	e.last[session] = now
	return true
}

// consumptionEscalations returns the daemon's escalator, creating it on first
// use. Lazily built so a Daemon literal constructed in a test needs no setup.
func (d *Daemon) consumptionEscalations() *consumptionEscalator {
	d.consumptionOnce.Do(func() {
		d.consumption = &consumptionEscalator{}
	})
	return d.consumption
}

// runningAgent names a session the daemon found already running, along with
// the identity needed to log it and to name the command that restarts it.
type runningAgent struct {
	// Role is the human-facing role name: "witness" or "refinery".
	Role string
	// Rig owns the session, and is what the restart commands take.
	Rig string
	// Session is the tmux session name the probe reads.
	Session string
}

// restartHint names the command that restarts this exact session. Both roles
// have a rig-scoped restart, and naming the right one matters: 'gt session
// restart' only handles polecats, so it would fail on either of these.
func (a runningAgent) restartHint() string {
	return fmt.Sprintf("gt %s restart %s", a.Role, a.Rig)
}

// probeRunningWitness and probeRunningRefinery are the entry points for the
// heartbeat's already-running paths: they resolve the session name the way the
// rest of the daemon does and hand it to the probe.
func (d *Daemon) probeRunningWitness(rigName string) {
	d.checkAgentInputConsumption(runningAgent{
		Role:    "witness",
		Rig:     rigName,
		Session: session.WitnessSessionName(session.PrefixFor(rigName)),
	})
}

func (d *Daemon) probeRunningRefinery(rigName string) {
	d.checkAgentInputConsumption(runningAgent{
		Role:    "refinery",
		Rig:     rigName,
		Session: session.RefinerySessionName(session.PrefixFor(rigName)),
	})
}

// checkAgentInputConsumption probes a session that the daemon found already
// running for the wedged state: alive, but consuming no input.
//
// It never returns an error: this runs on the heartbeat path, where a failed
// probe is a log line, not a failure of the tick.
func (d *Daemon) checkAgentInputConsumption(agent runningAgent) {
	d.probeInputConsumption(agent, d.escalate)
}

// probeInputConsumption is checkAgentInputConsumption with the escalation sink
// injected, so a test can assert what would be escalated without shelling out
// to `gt escalate`.
func (d *Daemon) probeInputConsumption(agent runningAgent, escalate func(source, message string)) {
	if d.tmux == nil || agent.Session == "" {
		return
	}

	frozenFor := config.LoadOperationalConfig(d.config.TownRoot).
		GetWitnessConfig().ComposerStallFrozenForD()

	stall, err := d.tmux.DetectComposerStall(agent.Session, frozenFor)
	if err != nil {
		// The pane could not be read. That says nothing about the session —
		// staying quiet is the only honest outcome.
		d.logger.Printf("%s consumption probe could not read %s: %v", agent.Role, agent.Session, err)
		return
	}
	if !stall.Stalled {
		return
	}

	d.logger.Printf("%s %s is alive but consuming no input: %s (no pane output for %s) — submitting its pending input",
		agent.Role, agent.Session, stall.Evidence, stall.Inactivity.Round(time.Second))

	if err := d.tmux.SubmitPendingInput(agent.Session, stall.Queued); err != nil {
		d.logger.Printf("%s consumption probe could not submit pending input to %s: %v", agent.Role, agent.Session, err)
		return
	}

	if consumptionRecheckDelay > 0 {
		time.Sleep(consumptionRecheckDelay)
	}

	after, err := d.tmux.DetectComposerStall(agent.Session, frozenFor)
	if err != nil {
		d.logger.Printf("%s consumption probe could not re-read %s after submitting: %v", agent.Role, agent.Session, err)
		return
	}
	if !after.Stalled {
		d.logger.Printf("%s %s recovered: pending input was submitted and the session is consuming it again",
			agent.Role, agent.Session)
		return
	}

	if escalate == nil {
		return
	}
	if !d.consumptionEscalations().shouldEscalate(agent.Session, time.Now()) {
		return
	}

	escalate("agent-consumption", fmt.Sprintf(
		"%s %s is alive but consuming no input, and submitting its pending input (%s) did not clear it. "+
			"Every process-level liveness check reports it running, so the daemon keeps skipping its spawn. "+
			"It is a wedged session, not a dead one: restart it with '%s'. "+
			"Paste 'gt session health %s' into a report if it recurs.",
		agent.Role, agent.Session, after.Evidence, agent.restartHint(), agent.Session))
}
