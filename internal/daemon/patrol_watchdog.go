package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/guard"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
)

// Patrol watchdog: flags a patrol role (witness, deacon) that is
// awake but NOT patrolling — its session is alive, but its last COMPLETED
// patrol cycle is older than N x its cadence (gt-4z3b7).
//
// Two independent mechanisms on 2026-09-23 produced this exact shape and both
// looked "alive" to every existing check: the witness read a nudge's Escape
// as an operator interrupt and sat paused for hours (gt-cyyg); the deacon's
// inbox processing consumed every turn, so its patrol molecule stayed hooked
// and never reached `gt patrol report`. Neither role logged a crash — both
// simply stopped advancing their patrol cycle while continuing to answer
// nudges in seconds. Session-liveness checks (zombie/stall detection) cannot
// see this: the session genuinely is alive. This watchdog is the daemon-side
// detector that does not depend on the stuck role's own loop to notice it.
const (
	defaultPatrolWatchdogInterval   = 10 * time.Minute
	defaultPatrolWatchdogCadence    = 10 * time.Minute
	defaultPatrolWatchdogMultiplier = 3
)

func patrolWatchdogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.PatrolWatchdog != nil {
		if config.Patrols.PatrolWatchdog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.PatrolWatchdog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultPatrolWatchdogInterval
}

func patrolWatchdogCadence(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.PatrolWatchdog != nil {
		if config.Patrols.PatrolWatchdog.CadenceStr != "" {
			if d, err := time.ParseDuration(config.Patrols.PatrolWatchdog.CadenceStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultPatrolWatchdogCadence
}

func patrolWatchdogMultiplier(config *DaemonPatrolConfig) int {
	if config != nil && config.Patrols != nil && config.Patrols.PatrolWatchdog != nil && config.Patrols.PatrolWatchdog.Multiplier > 0 {
		return config.Patrols.PatrolWatchdog.Multiplier
	}
	return defaultPatrolWatchdogMultiplier
}

func patrolWatchdogNudgeEnabled(config *DaemonPatrolConfig) bool {
	if config != nil && config.Patrols != nil && config.Patrols.PatrolWatchdog != nil && config.Patrols.PatrolWatchdog.Nudge != nil {
		return *config.Patrols.PatrolWatchdog.Nudge
	}
	return true
}

// patrolWatchdogTarget names one patrol role instance to check: a specific
// rig's witness or refinery, or the town-level deacon.
type patrolWatchdogTarget struct {
	Role      string // "witness", "deacon"
	Rig       string // "" for town-level roles
	Session   string // tmux session name
	Assignee  string // bd assignee address (witness.PatrolAssignee)
	PatrolMol string // patrol molecule name (e.g. "mol-witness-patrol")
	// WorkDir is the bd working directory for this role's patrol wisps: the
	// TOWN root for every role, rig-scoped ones included. Before editing this
	// field, read gt patrol report's config (internal/cmd/patrol_report.go) —
	// it writes patrol wisps with BeadsDir=roleInfo.TownRoot, so they live in
	// the town database (hq-), never in the role's rig database. A rig path
	// resolves through <rig>/.beads/redirect to <rig>/mayor/rig/.beads, where
	// the query finds no patrol wisp: a bare [] that read as a trustworthy
	// "never patrolled" and alarmed on every healthy role (hq-3h7ac).
	WorkDir string
}

// patrolWatchdogTargets enumerates every patrol role instance the watchdog
// checks: the town-level deacon plus each known rig's witness.
func patrolWatchdogTargets(townRoot string, rigs []string) []patrolWatchdogTarget {
	targets := []patrolWatchdogTarget{{
		Role:      constants.RoleDeacon,
		Session:   session.DeaconSessionName(),
		Assignee:  witness.PatrolAssignee(constants.RoleDeacon, ""),
		PatrolMol: constants.MolDeaconPatrol,
		WorkDir:   townRoot,
	}}

	for _, rigName := range rigs {
		prefix := config.GetRigPrefix(townRoot, rigName)

		targets = append(targets,
			patrolWatchdogTarget{
				Role:      constants.RoleWitness,
				Rig:       rigName,
				Session:   session.WitnessSessionName(prefix),
				Assignee:  witness.PatrolAssignee(constants.RoleWitness, rigName),
				PatrolMol: constants.MolWitnessPatrol,
				WorkDir:   townRoot,
			},
		)
	}
	return targets
}

// filterPatrolWatchdogTargets drops the roles the town has switched off: the
// deacon when its patrol is disabled, and the witness of a rig where
// WitnessWantedInRig is false. A role deliberately removed is not "awake but
// not patrolling", and alarming on it every cycle would bury real findings
// (ADR 0005).
func filterPatrolWatchdogTargets(targets []patrolWatchdogTarget, cfg *DaemonPatrolConfig, active func(string) bool) []patrolWatchdogTarget {
	out := targets[:0:0]
	for _, t := range targets {
		switch t.Role {
		case constants.RoleDeacon:
			if !active(constants.RoleDeacon) {
				continue
			}
		case constants.RoleWitness:
			if !active(constants.RoleWitness) || !WitnessWantedInRig(cfg, t.Rig) {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// pausedRig is a known rig the watchdog will not check, together with the
// operational state that put it out of reach.
type pausedRig struct {
	Rig   string
	State rig.OpState
}

// partitionPausedRigs splits the known rigs into the ones the watchdog should
// check and the ones an operator has paused.
//
// A paused rig has no witness and no refinery to check: `gt rig park` stops
// both, so the sessions the watchdog would query cannot exist. Querying them
// anyway was not harmless. Every cycle escalated "awake but NOT patrolling" for
// rigs nobody had started — the liveness read answered alive for a session that
// outlived the park, while the patrol receipts confirmed a cycle never
// completed — and then ran `gt nudge` on a session that really was gone. gt's
// refusal carried its whole usage block, which daemon.log repeated for beads,
// om, hm and mango every cycle until `gt tail` was unreadable (gt-7g14a).
//
// Docked rigs are skipped too, not just parked ones: PARKED and DOCKED are the
// two states in which the town has decided no agent runs, and the two states in
// which `isRigOperational` refuses to auto-start one.
//
// opState is injected so tests can drive the split with no town on disk. The
// daemon passes rig.GetOpState — the resolver `gt rig list` and the dashboard
// share, which reads the wisp layer `gt rig park` writes and then the rig
// identity bead's labels as the persistent fallback — rather than parsing the
// wisp JSON here and drifting from the CLI's idea of parked. A read it could
// not answer reports the rig operational, so a failed read leaves a rig inside
// the watchdog's reach; the one outcome that must not happen is a live rig
// dropping silently out of the patrol that exists to notice its silence.
func partitionPausedRigs(rigs []string, opState func(rigName string) (rig.OpState, string)) (active []string, paused []pausedRig) {
	for _, rigName := range rigs {
		state, _ := opState(rigName)
		if state == rig.OpStateOperational {
			active = append(active, rigName)
			continue
		}
		paused = append(paused, pausedRig{Rig: rigName, State: state})
	}
	return active, paused
}

// patrolWatchdogPausedLogInterval bounds how often the watchdog repeats its
// skip line for the same rig in the same state. The watchdog runs every cycle
// forever, so without a bound a town with four parked rigs writes four
// identical lines every cycle — the same log-filling shape gt-7g14a exists to
// remove. A state change is reported at once regardless of the interval.
const patrolWatchdogPausedLogInterval = time.Hour

// pausedRigSeen is the last skip notice written for one rig.
type pausedRigSeen struct {
	state rig.OpState
	at    time.Time
}

// pausedRigLog throttles the skip notice to at most one line per rig per
// patrolWatchdogPausedLogInterval, and remembers enough to tell the first
// notice for a state from a repeat of it. The zero value is ready to use.
type pausedRigLog struct {
	mu   sync.Mutex
	seen map[string]pausedRigSeen
}

// observe records the paused state just read for rigName and reports whether a
// skip notice is due now, and whether this is the rig's first notice in that
// state — the transition the caller clears the rig's stale patrol alert on.
func (l *pausedRigLog) observe(rigName string, state rig.OpState, now time.Time) (notice, entered bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]pausedRigSeen)
	}

	prev, ok := l.seen[rigName]
	if ok && prev.state == state && now.Sub(prev.at) < patrolWatchdogPausedLogInterval {
		return false, false
	}
	l.seen[rigName] = pausedRigSeen{state: state, at: now}
	return true, !ok || prev.state != state
}

// forget drops a rig's entry, so a rig parked again later reads as a fresh
// transition instead of being suppressed by the interval that covered its
// previous, already-cleared park.
func (l *pausedRigLog) forget(rigName string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, rigName)
}

// patrolWatchdogFinding is one evaluated target: the guard.Result carries the
// verdict (Pass/Fail/Unknown), never collapsed to a bool, so a caller cannot
// treat "not Fail" as "healthy" (gt-udrrw).
type patrolWatchdogFinding struct {
	Target patrolWatchdogTarget
	Result guard.Result
}

// patrolWatchdogSessionAlive is the watchdog's liveness reader. A failed
// query is unknown, not dead: a dead session passes the watchdog outright, so
// unknown reads as alive and the patrol is judged by its receipts (gt-fcxe9.1).
//
// A session that does not exist is a different answer from a query that
// failed. The one question this reader answers is whether an agent is alive
// in the session, and "there is no session" answers it; treating that as
// unknown read a gone role as awake-but-silent and then nudged the session it
// had just called alive, which `gt nudge` refused as nonexistent (gt-jv0k3).
// tmux reports the missing case as ErrSessionNotFound (internal/tmux
// wrapError), so there is nothing here to judge by receipts.
func (d *Daemon) patrolWatchdogSessionAlive(target patrolWatchdogTarget) bool {
	alive, err := d.tmux.IsAgentAliveChecked(target.Session)
	if err != nil {
		if errors.Is(err, tmux.ErrSessionNotFound) {
			d.logger.Printf("patrol_watchdog: %s has no session (%v); nothing to judge", target.Session, err)
			return false
		}
		d.logger.Printf("patrol_watchdog: %s liveness unknown (%v); judging by receipts", target.Session, err)
		return true
	}
	return alive
}

// assessPatrolWatchdogTargets evaluates every target against injected
// liveness/receipt readers. It has no side effects (no escalation, no
// nudging, no mail) — it is the pure core that unit tests drive directly to
// prove the alarm fires, independent of tmux/bd/mail wiring.
func assessPatrolWatchdogTargets(
	targets []patrolWatchdogTarget,
	sessionAlive func(target patrolWatchdogTarget) bool,
	lastCompleted func(target patrolWatchdogTarget) (time.Time, guard.Result),
	cadence time.Duration,
	multiplier int,
	now time.Time,
) []patrolWatchdogFinding {
	findings := make([]patrolWatchdogFinding, 0, len(targets))
	for _, target := range targets {
		alive := sessionAlive(target)

		var completedAt time.Time
		var completedResult guard.Result
		if alive {
			// Only pay for the bd read when it can actually change the
			// verdict — EvaluatePatrolLiveness Passes immediately for a dead
			// session regardless of receipt state.
			completedAt, completedResult = lastCompleted(target)
		}

		result := witness.EvaluatePatrolLiveness(witness.PatrolLivenessInput{
			Role:                target.Role,
			Rig:                 target.Rig,
			SessionAlive:        alive,
			LastCompleted:       completedAt,
			LastCompletedResult: completedResult,
			Cadence:             cadence,
			Multiplier:          multiplier,
			Now:                 now,
		})

		findings = append(findings, patrolWatchdogFinding{Target: target, Result: result})
	}
	return findings
}

// patrolWatchdogAlertKey is the escalateAlert/clearAlerts fingerprint for one
// target, stable across ticks so repeated Fail verdicts don't mint repeated
// escalations and a later Pass clears exactly the alert that was raised.
func patrolWatchdogAlertKey(target patrolWatchdogTarget) string {
	if target.Rig == "" {
		return "patrol_watchdog:" + target.Role
	}
	return "patrol_watchdog:" + target.Rig + "/" + target.Role
}

// patrolWatchdogRigAlertKeys returns the alert keys the watchdog can raise for
// a rig's two patrol roles, so a rig that leaves the watchdog's reach can close
// both.
func patrolWatchdogRigAlertKeys(rigName string) []string {
	return []string{
		patrolWatchdogAlertKey(patrolWatchdogTarget{Role: constants.RoleWitness, Rig: rigName}),
	}
}

// reportPausedRigs logs the rigs this cycle skipped and clears their patrol
// alerts on the transition into a paused state.
//
// The clear is what keeps a skip from becoming a permanent alarm. The alert key
// is normally cleared by the Pass verdict a resumed patrol produces, but a
// paused rig is never judged again, so an alert raised while it was still being
// watched — exactly the "awake but NOT patrolling" escalation gt-7g14a reports
// for hm and mango — would otherwise stay open with no verdict ever coming to
// close it. Parking a rig is the town's decision that nothing there should be
// running, so the alert is stale the moment the rig is paused, not evidence.
func (d *Daemon) reportPausedRigs(active []string, paused []pausedRig) {
	for _, rigName := range active {
		d.patrolWatchdogPaused.forget(rigName)
	}

	now := time.Now()
	for _, p := range paused {
		notice, entered := d.patrolWatchdogPaused.observe(p.Rig, p.State, now)
		if !notice {
			continue
		}
		d.logger.Printf("patrol_watchdog: skipping %s: rig is %s, no agents to check", p.Rig, p.State.Label())
		if entered {
			d.clearAlerts("rig is "+p.State.Label(), patrolWatchdogRigAlertKeys(p.Rig)...)
		}
	}
}

// patrolWatchdogEscalationMessage builds the mail body for a Fail verdict.
func patrolWatchdogEscalationMessage(finding patrolWatchdogFinding) string {
	label := finding.Target.Role
	if finding.Target.Rig != "" {
		label = finding.Target.Rig + "/" + finding.Target.Role
	}
	var reason string
	if err := finding.Result.Err(); err != nil {
		reason = err.Error()
	}
	return fmt.Sprintf(
		"Patrol watchdog: %s is awake (session %s is alive) but NOT patrolling.\n\n%s\n\n"+
			"This role is answering nudges but has not completed a patrol cycle recently — "+
			"the same shape as an interrupt read as a user stop (gt-cyyg) or callback "+
			"starvation consuming every turn. Investigate the live session directly; "+
			"a nudge alone may not be enough if the loop itself is stuck.",
		label, finding.Target.Session, reason,
	)
}

// triggerPatrolWatchdog runs one patrol_watchdog cycle on its own goroutine
// (single-flight): each target's bd read and tmux liveness check, plus any
// `gt escalate`/`gt nudge` subprocess a Fail verdict triggers, can take real
// wall-clock time, and running them inline would hold the tick loop.
//
// The ticker that drives this call is a check cadence, not a run cadence: an
// in-process ticker resets its countdown on every daemon restart, so due-ness
// is instead decided from the persisted last-run time in
// daemon/patrol_last_run.json, which survives a restart (gt-ima2, gt-gxpwc).
func (d *Daemon) triggerPatrolWatchdog() {
	// d.config may be nil in unit tests that exercise only the single-flight
	// guard; without a TownRoot there is no last-run file to consult, so the
	// check runs unconditionally rather than dereferencing a nil config.
	dec := patrolDueDecision{due: true, note: "no town root available — running unconditionally"}
	if d.config != nil {
		dec = evaluatePatrolDue(d.config.TownRoot, "patrol_watchdog", time.Time{}, time.Now(), patrolWatchdogInterval(d.patrolConfig))
	}
	if !dec.due {
		d.logger.Printf("patrol_watchdog: not due — %s", dec.note)
		return
	}

	if !d.patrolWatchdogRunning.CompareAndSwap(false, true) {
		d.logger.Printf("patrol_watchdog: previous cycle still running, skipping this tick")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("patrol_watchdog: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("patrol_watchdog: due — %s", dec.note)
	}

	go func() {
		defer d.patrolWatchdogRunning.Store(false)
		d.runPatrolWatchdog()
		if d.config == nil {
			return
		}
		if err := savePatrolLastRun(d.config.TownRoot, "patrol_watchdog", time.Now()); err != nil {
			d.logger.Printf("patrol_watchdog: WARNING: cannot persist last-run time (%v) — "+
				"the next check may re-run sooner than expected", err)
		}
	}()
}

// runPatrolWatchdog checks every known patrol role instance and escalates any
// that are awake but not patrolling. Wired from the daemon ticker
// (isPatrolActive("patrol_watchdog")).
func (d *Daemon) runPatrolWatchdog() {
	if !d.isPatrolActive("patrol_watchdog") {
		return
	}

	rigs, paused := partitionPausedRigs(d.getKnownRigs(), func(rigName string) (rig.OpState, string) {
		return rig.GetOpState(d.config.TownRoot, rigName)
	})
	d.reportPausedRigs(rigs, paused)

	targets := filterPatrolWatchdogTargets(patrolWatchdogTargets(d.config.TownRoot, rigs), d.patrolConfig, d.isPatrolActive)
	cadence := patrolWatchdogCadence(d.patrolConfig)
	multiplier := patrolWatchdogMultiplier(d.patrolConfig)
	nudgeEnabled := patrolWatchdogNudgeEnabled(d.patrolConfig)

	bd := witness.DefaultBdCli()

	findings := assessPatrolWatchdogTargets(
		targets,
		d.patrolWatchdogSessionAlive,
		func(target patrolWatchdogTarget) (time.Time, guard.Result) {
			return witness.LastCompletedPatrol(bd, target.WorkDir, target.Assignee, target.PatrolMol)
		},
		cadence, multiplier, time.Now(),
	)

	for _, finding := range findings {
		key := patrolWatchdogAlertKey(finding.Target)
		switch {
		case finding.Result.IsFail():
			d.logger.Printf("patrol_watchdog: %s — %s", key, finding.Result)
			d.escalateAlert(key, "patrol_watchdog", patrolWatchdogEscalationMessage(finding))
			if nudgeEnabled {
				d.nudgeStalePatrol(finding.Target)
			}
		case finding.Result.IsPass():
			d.clearAlerts("patrol resumed", key)
		default: // Unknown: never treated as healthy, but also not spammed as a
			// confirmed alarm — log for visibility and leave any existing
			// alert state untouched until the next read succeeds either way.
			d.logger.Printf("patrol_watchdog: %s — %s", key, finding.Result)
		}
	}
}

// nudgeStalePatrol sends a "resume patrol" nudge to a stale-but-alive role's
// session via `gt nudge`, the town's one nudge path (default wait-idle mode:
// it queues rather than interrupting a busy pane, and — since gt-cyyg —
// never sends Escape to a Claude Code session). Best-effort: escalation
// above is what guarantees the finding is seen even if the nudge is dropped
// or queued behind other work.
func (d *Daemon) nudgeStalePatrol(target patrolWatchdogTarget) {
	address := target.Role + "/"
	if target.Rig != "" {
		address = target.Rig + "/" + target.Role
	}

	message := "resume patrol: your last completed patrol cycle is stale (see escalation). " +
		"Run `gt patrol report` to close out the current cycle, or investigate why the loop stalled."

	if err := d.nudgeSession(address, message); err != nil {
		d.logger.Printf("patrol_watchdog: nudge to %s failed (non-fatal): %v", address, err)
	}
}

// nudgeSession delivers a message via `gt nudge <address> <message>` — the
// same subprocess path runMayorDispatch's nudgeMayor uses. Default (wait-idle)
// mode queues rather than interrupts a busy pane, and — since gt-cyyg —
// EscapeCancelsRequest keeps Claude Code sessions from reading it as an
// operator stop.
func (d *Daemon) nudgeSession(address, message string) error {
	ctx, cancel := context.WithTimeout(d.ctx, mayorNudgeTimeout)
	defer cancel()
	return d.notify().Nudge(ctx, address, message)
}
