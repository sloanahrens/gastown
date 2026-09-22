package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
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
// The probe itself is the already-reviewed one: tmux.DetectComposerStallTracked
// (gt-hkhu), which requires unsubmitted input in the pane AND a clock that has
// run out on it — the silence window, or the pending-input age (gt-afa7). That
// conjunction is what keeps it off a healthy idle-await agent carrying a
// legitimately queued nudge: pane shape alone cannot tell the two apart, so the
// age clock only counts a run that several observations, a stable session id,
// and an unchanging pane transcript all agree on.
//
// Every branch leaves a trace. A probe that ran and declined to act reports
// what it saw; silence made a 17-minute stall identical to a healthy session in
// the log (gt-afa7).
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
	// unclassified rate-limits the "could not classify" log line separately
	// from the escalation: they answer different questions and suppressing one
	// must not suppress the other.
	unclassified map[string]time.Time
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

// shouldLogUnclassifiable reports whether an unclassifiable pane should be
// logged for this session now, recording the time when it returns true. The
// first occurrence always logs; repeats are held to the escalation interval.
func (e *consumptionEscalator) shouldLogUnclassifiable(session string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.unclassified == nil {
		e.unclassified = make(map[string]time.Time)
	}
	if last, ok := e.unclassified[session]; ok && now.Sub(last) < consumptionEscalationInterval {
		return false
	}
	e.unclassified[session] = now
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

// pendingInputClock returns the daemon's pending-input clock, creating it on
// first use. A daemon with no town root gets a disabled clock, which leaves the
// probe on the silence window alone (gt-afa7).
func (d *Daemon) pendingInputClock() *tmux.PendingInputClock {
	d.pendingClockOnce.Do(func() {
		if d.config == nil {
			return
		}
		d.pendingClock = tmux.NewPendingInputClock(constants.TownRuntimePath(d.config.TownRoot))
	})
	return d.pendingClock
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

	clock := d.pendingInputClock()
	stall, err := d.tmux.DetectComposerStallTracked(agent.Session, frozenFor, clock)
	if err != nil {
		// The pane could not be read, or was read and could not be classified.
		// Nothing is known about the session but the failure, so do not act:
		// escalating on "I could not look" would fire forever on any agent whose
		// TUI the probe cannot parse.
		//
		// It is still reported — a pane nobody can classify must not be
		// indistinguishable from a healthy one — but on a cooldown, because the
		// condition persists across heartbeats (a modal, an agent with no prompt
		// prefix) and one line every four minutes per agent buries the lines
		// that matter (gt-afa7).
		if d.consumptionEscalations().shouldLogUnclassifiable(agent.Session, time.Now()) {
			d.logger.Printf("%s consumption probe could not classify %s: %v", agent.Role, agent.Session, err)
		}
		return
	}
	if !stall.Stalled {
		// Input is waiting but no clock has run out. Say so, with the age: this
		// branch used to return silently, leaving the stall of gt-afa7
		// indistinguishable in the log from a healthy session.
		if stall.State == tmux.ComposerPending {
			d.logger.Printf("%s %s is holding unsubmitted input (%s) — waiting %s over %d observation(s), no pane output for %s, threshold %s; still watching",
				agent.Role, agent.Session, stall.Evidence,
				stall.PendingFor.Round(time.Second), stall.PendingSamples,
				stall.Inactivity.Round(time.Second), frozenFor)
		}
		return
	}

	// Evidence carries the timing on every branch, including a stall tripped by
	// silence alone — the branch that used to log no duration at all (gt-afa7).
	d.logger.Printf("%s %s is alive but consuming no input: %s — submitting its pending input",
		agent.Role, agent.Session, stall.Evidence)

	if err := d.tmux.SubmitPendingInput(agent.Session, stall.Queued); err != nil {
		d.logger.Printf("%s consumption probe could not submit pending input to %s: %v", agent.Role, agent.Session, err)
		return
	}
	// The wait is over now that someone has intervened: clear the pending clock
	// so the re-probe judges the flush on the silence window alone.
	clock.Reset(agent.Session)

	if consumptionRecheckDelay > 0 {
		time.Sleep(consumptionRecheckDelay)
	}

	after, err := d.tmux.DetectComposerStallTracked(agent.Session, frozenFor, clock)
	if err != nil {
		d.logger.Printf("%s consumption probe could not re-classify %s after submitting: %v", agent.Role, agent.Session, err)
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
