package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/guard"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/witness"
)

// Patrol watchdog: flags a patrol role (witness, deacon, refinery) that is
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

// PatrolWatchdogConfig holds configuration for the patrol_watchdog patrol.
type PatrolWatchdogConfig struct {
	// Enabled controls whether the patrol runs. Defaults to true (see
	// IsPatrolEnabled) — an explicit false is required to turn it off.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often the watchdog checks, as a string (e.g. "10m").
	IntervalStr string `json:"interval,omitempty"`

	// CadenceStr is the expected time between a role's completed patrol
	// cycles, as a string (e.g. "10m"). Applied uniformly to every role;
	// per-role overrides can be added later if cadences diverge.
	CadenceStr string `json:"cadence,omitempty"`

	// Multiplier is how many cadences of silence are tolerated before a role
	// is considered stale. Zero means "use the default" (3).
	Multiplier int `json:"multiplier,omitempty"`

	// Nudge controls whether a stale-but-alive role is also sent a "resume
	// patrol" nudge in addition to being escalated. Defaults to true.
	Nudge *bool `json:"nudge,omitempty"`
}

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
	Role      string // "witness", "deacon", "refinery"
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
// checks: the town-level deacon plus each known rig's witness and refinery.
func patrolWatchdogTargets(townRoot string, rigs []string) []patrolWatchdogTarget {
	targets := []patrolWatchdogTarget{{
		Role:      constants.RoleDeacon,
		Session:   session.DeaconSessionName(),
		Assignee:  witness.PatrolAssignee(constants.RoleDeacon, ""),
		PatrolMol: constants.MolDeaconPatrol,
		WorkDir:   townRoot,
	}}

	for _, rig := range rigs {
		prefix := config.GetRigPrefix(townRoot, rig)

		targets = append(targets,
			patrolWatchdogTarget{
				Role:      constants.RoleWitness,
				Rig:       rig,
				Session:   session.WitnessSessionName(prefix),
				Assignee:  witness.PatrolAssignee(constants.RoleWitness, rig),
				PatrolMol: constants.MolWitnessPatrol,
				WorkDir:   townRoot,
			},
			patrolWatchdogTarget{
				Role:      constants.RoleRefinery,
				Rig:       rig,
				Session:   session.RefinerySessionName(prefix),
				Assignee:  witness.PatrolAssignee(constants.RoleRefinery, rig),
				PatrolMol: constants.MolRefineryPatrol,
				WorkDir:   townRoot,
			},
		)
	}
	return targets
}

// patrolWatchdogFinding is one evaluated target: the guard.Result carries the
// verdict (Pass/Fail/Unknown), never collapsed to a bool, so a caller cannot
// treat "not Fail" as "healthy" (gt-udrrw).
type patrolWatchdogFinding struct {
	Target patrolWatchdogTarget
	Result guard.Result
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

	rigs := d.getKnownRigs()
	targets := patrolWatchdogTargets(d.config.TownRoot, rigs)
	cadence := patrolWatchdogCadence(d.patrolConfig)
	multiplier := patrolWatchdogMultiplier(d.patrolConfig)
	nudgeEnabled := patrolWatchdogNudgeEnabled(d.patrolConfig)

	bd := witness.DefaultBdCli()

	findings := assessPatrolWatchdogTargets(
		targets,
		func(target patrolWatchdogTarget) bool {
			return d.tmux.IsAgentAlive(target.Session)
		},
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

	cmd := exec.CommandContext(ctx, d.gtPath, "nudge", address, message) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
