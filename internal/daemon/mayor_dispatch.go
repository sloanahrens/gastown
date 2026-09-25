package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

const (
	// defaultMayorDispatchInterval is how often the idle-seat patrol runs.
	// Half an hour is the cadence the operator's launchd stopgap used, and it
	// is slow enough that a town with no free seat and no ready work is not
	// asked the same question repeatedly within one dispatch cycle (a pier
	// sling plus a polecat's run is typically 10-40 minutes).
	defaultMayorDispatchInterval = 30 * time.Minute

	// mayorDispatchTimeout bounds the `gt daemon dispatch-check` subprocess.
	// The check reads the rig registry, live tmux sessions, and up to two bead
	// queries per rig; two minutes is generous for that and bounded enough
	// that a wedged Dolt does not strand the patrol.
	mayorDispatchTimeout = 2 * time.Minute

	// mayorNudgeTimeout bounds `gt nudge`. Its own wait-idle mode falls back to
	// queueing after 15s, so this is the subprocess's startup and teardown
	// budget, not a second delivery window.
	mayorNudgeTimeout = 60 * time.Second
)

// MayorDispatchConfig holds configuration for the mayor_dispatch patrol.
type MayorDispatchConfig struct {
	// Enabled controls whether the patrol runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to check, as a string (e.g., "30m").
	IntervalStr string `json:"interval,omitempty"`
}

// mayorDispatchInterval returns the configured interval, or the default (30m).
func mayorDispatchInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.MayorDispatch != nil {
		if config.Patrols.MayorDispatch.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.MayorDispatch.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultMayorDispatchInterval
}

// dispatchCheckResult is the slice of `gt daemon dispatch-check --json` the
// patrol reads. The decision and the nudge text are computed there, next to the
// seat model and the merge-queue rule they come from (internal/cmd), so this
// side only carries the answer.
type dispatchCheckResult struct {
	Seats struct {
		Free int `json:"free"`
	} `json:"seats"`
	Actionable int    `json:"actionable"`
	Nudge      bool   `json:"nudge"`
	Message    string `json:"message"`
}

// triggerMayorDispatch runs one idle-seat cycle on its own goroutine. The check
// shells out (a subprocess that reads Dolt and tmux) and then nudges, which can
// each take tens of seconds; running either inline would hold the tick loop.
// Same shape as triggerMainBranchTests (gt-uvxy, gt-59o9).
//
// The ticker that drives this call is a check cadence, not a run cadence: an
// in-process ticker resets its countdown on every daemon restart, so due-ness
// is instead decided from the persisted last-run time in
// daemon/patrol_last_run.json, which survives a restart (gt-ima2, gt-gxpwc).
func (d *Daemon) triggerMayorDispatch() bool {
	// d.config is nil in unit tests that exercise only the single-flight
	// guard below (e.g. with the patrol disabled, so runMayorDispatch
	// returns immediately without shelling out); without a TownRoot there is
	// no last-run file to consult, so the check runs unconditionally rather
	// than dereferencing a nil config.
	dec := patrolDueDecision{due: true, note: "no town root available — running unconditionally"}
	if d.config != nil {
		dec = evaluatePatrolDue(d.config.TownRoot, "mayor_dispatch", time.Time{}, time.Now(), mayorDispatchInterval(d.patrolConfig))
	}
	if !dec.due {
		d.logger.Printf("mayor_dispatch: not due — %s", dec.note)
		return false
	}

	if !d.mayorDispatchRunning.CompareAndSwap(false, true) {
		d.logger.Printf("mayor_dispatch: previous cycle still running, skipping this tick")
		return false
	}

	if dec.warn != "" {
		d.logger.Printf("mayor_dispatch: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("mayor_dispatch: due — %s", dec.note)
	}

	go func() {
		defer d.mayorDispatchRunning.Store(false)
		d.runMayorDispatch()
		if d.config == nil {
			return
		}
		if err := savePatrolLastRun(d.config.TownRoot, "mayor_dispatch", time.Now()); err != nil {
			d.logger.Printf("mayor_dispatch: WARNING: cannot persist last-run time (%v) — "+
				"the next check may re-run sooner than expected", err)
		}
	}()
	return true
}

// runMayorDispatch asks whether the town has free polecat seats and ready work,
// and nudges the mayor when it does.
//
// The mayor is purely event-driven: it wakes on a slot opening, an escalation,
// or mail. Once it declines to dispatch and no polecat is left running, no
// further slot opens and nothing wakes it again — on 2026-09-21 the town sat
// idle 03:41-09:07 with 362 ready beads on the board. This patrol is the timer
// that decision does not self-provide.
//
// It never slings. Dispatch stays the mayor's call: the patrol's job is to make
// sure the call gets made, and a daemon that dispatched on its own would be a
// second, dumber scheduler.
func (d *Daemon) runMayorDispatch() {
	if !d.isPatrolActive("mayor_dispatch") {
		return
	}

	d.logger.Printf("mayor_dispatch: starting cycle")

	mol := d.pourDogMolecule(constants.MolDogMayorDispatch, nil)
	defer mol.close()

	result, err := d.readDispatchCheck()
	if err != nil {
		d.logger.Printf("mayor_dispatch: check failed: %v", err)
		mol.failStep("inspect", err.Error())
		return
	}
	mol.closeStep("inspect")

	if !result.Nudge {
		// The silence cases (no seat model, no free seat, no actionable work
		// outside a held rig) are all one outcome here, and the reason is in
		// the check's own output — say the numbers so a patrol that never
		// fires is visible in the log rather than inferred from silence.
		d.logger.Printf("mayor_dispatch: no nudge — %d free seat(s), %d actionable ready bead(s)",
			result.Seats.Free, result.Actionable)
		mol.skipStep("nudge", fmt.Sprintf("no nudge warranted: %d free seat(s), %d actionable ready bead(s)",
			result.Seats.Free, result.Actionable))
		mol.closeStep("report")
		return
	}

	d.logger.Printf("mayor_dispatch: nudging mayor — %d free seat(s), %d actionable ready bead(s)",
		result.Seats.Free, result.Actionable)
	if err := d.nudgeMayor(result.Message); err != nil {
		d.logger.Printf("mayor_dispatch: nudge failed: %v", err)
		mol.failStep("nudge", err.Error())
		mol.closeStep("report")
		return
	}
	mol.closeStep("nudge")
	mol.closeStep("report")
	d.logger.Printf("mayor_dispatch: cycle complete")
}

// readDispatchCheck runs `gt daemon dispatch-check --json` and parses it.
func (d *Daemon) readDispatchCheck() (*dispatchCheckResult, error) {
	ctx, cancel := context.WithTimeout(d.ctx, mayorDispatchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.gtPath, "daemon", "dispatch-check", "--json") //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}

	return parseDispatchCheck(stdout.Bytes())
}

// parseDispatchCheck reads the check's JSON, and refuses an output that carries
// no decision: a nudge fired on a struct that failed to populate would send the
// mayor an empty message, and `gt nudge` would refuse it — a failure the log
// should name as a parse problem, not as a delivery problem.
func parseDispatchCheck(out []byte) (*dispatchCheckResult, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, fmt.Errorf("dispatch check produced no output")
	}

	var result dispatchCheckResult
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("parsing dispatch check output: %w", err)
	}
	if result.Nudge && strings.TrimSpace(result.Message) == "" {
		return nil, fmt.Errorf("dispatch check asked for a nudge with an empty message")
	}
	return &result, nil
}

// nudgeMayor delivers the patrol's nudge to the mayor session.
//
// Delivery goes through `gt nudge`, which is the town's one nudge path: it
// queues when the mayor is busy rather than interrupting a turn, and it honors
// the mayor's DND setting — a nudge the target has asked not to receive is
// dropped by that path, and that is the correct outcome for a cadence.
func (d *Daemon) nudgeMayor(message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("refusing to nudge with an empty message")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mayorNudgeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.gtPath, "nudge", constants.RoleMayor, message) //nolint:gosec // G204: gtPath resolved at daemon init
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
