package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/patrolstate"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/witness"
)

// Witness session cycling (claude-8w7 step 3).
//
// With witness.cycle_session_at_idle_cap on in the rig root config.json,
// `gt patrol report` run by the rig's witness in its own pane respawns the
// witness session in place, after the report, when:
//
//   - this cycle's await-signal timed out at its backoff cap (a quiet rig: the
//     next cycle is a full cap interval away) and the session has completed
//     at least witness.cycle_session_min_cycles cycles (default 3); or
//   - the session has completed witness.cycle_session_max_cycles cycles
//     (default 8), whatever the wait did (the backstop for a busy rig).
//
// Everything the successor needs is already outside the session: stall
// sample 1 in stall_samples.json (step 1), idle:N on the agent bead, the
// patrol wisp, mail and queued nudges (await-signal leaves the queue to the
// report while cycling is on; see awaitSignalDrainNudges). The cycle counter and the wait outcome
// are Go-owned files under the witness directory (internal/patrolstate).

// witnessCycleParams is one `gt patrol report` by a witness.
type witnessCycleParams struct {
	Rig       string
	Session   string // session.WitnessSessionName(prefix)
	WorkDir   string // witness.WitnessStateDir: where the session runs and its .runtime lives
	Enabled   bool   // witness.cycle_session_at_idle_cap
	MinCycles int
	MaxCycles int // 0: no backstop
}

// witnessCycleDeps holds every side effect, so tests never touch tmux, beads,
// mail or the town.
type witnessCycleDeps struct {
	// Report closes the patrol cycle and pours the next (runPatrolReportFor).
	Report      func(drainNudges bool) error
	SessionID   func() string
	ReadWait    func() (*patrolstate.WaitOutcome, error)
	LoadState   func() (patrolstate.CycleState, error)
	SaveState   func(patrolstate.CycleState) error
	RecordCycle func() // cooldown timestamp + handoff marker + town log
	Respawn     func() error
	Escalate    func(fingerprint, severity, msg string)
	PaneSession func() (string, error)
	HandoffAge  func() (time.Duration, bool)
	Getenv      func(string) string
	Now         func() time.Time
	Out         io.Writer
}

// reportAndMaybeCycleWitness runs the witness's patrol report and then, when
// this cycle is a respawn boundary and every guard passes, persists the
// counter, records the cycle and respawns the witness pane, in that order.
// Every gate is decided before the report so that queued nudges are left in
// place (drainNudges=false) exactly when the session is about to be replaced.
// A report error is returned and the cycle is neither counted nor cycled.
func reportAndMaybeCycleWitness(p witnessCycleParams, d witnessCycleDeps) (unitCycleReport, error) {
	if !p.Enabled {
		// Flag off: exactly today's report, nothing counted, nothing printed.
		return unitCycleReport{SkipCause: "cycle disabled (witness.cycle_session_at_idle_cap off)"}, d.Report(true)
	}
	if cause := ownPaneCallerMismatch(p.Rig+"/witness", p.Session, d.Getenv, d.PaneSession); cause != "" {
		if err := d.Report(true); err != nil {
			return unitCycleReport{SkipCause: cause}, err
		}
		witnessSessionKept(d.Out, cause)
		return unitCycleReport{SkipCause: cause}, nil
	}

	prev, err := d.LoadState()
	if err != nil {
		fmt.Fprintf(d.Out, "  %s cycle counter unreadable, counting from 0: %v\n", style.Dim.Render("⚠"), err)
		prev = patrolstate.CycleState{}
	}
	wait, err := d.ReadWait()
	if err != nil {
		fmt.Fprintf(d.Out, "  %s await-signal outcome unreadable: %v\n", style.Dim.Render("⚠"), err)
		wait = nil
	}
	step := patrolstate.AdvanceCycle(prev, d.SessionID(), wait, d.Now())
	respawn, why := patrolstate.CycleBoundary(step.State.Cycles, step.Wait, p.MinCycles, p.MaxCycles)
	cause := ""
	if !respawn {
		cause = why
	} else if c := handoffCooldownCause(d.HandoffAge, "a later cycle respawns"); c != "" {
		cause = c
	}

	if err := d.Report(cause != ""); err != nil {
		return unitCycleReport{SkipCause: "patrol report failed"}, err
	}

	if cause != "" {
		if err := d.SaveState(step.State); err != nil {
			fmt.Fprintf(d.Out, "  %s cycle counter not saved: %v\n", style.Dim.Render("⚠"), err)
		}
		witnessSessionKept(d.Out, cause)
		return unitCycleReport{SkipCause: cause}, nil
	}

	// The successor starts at zero even when the runtime session ID is
	// unknown and would not change across the respawn. If that cannot be
	// persisted, keep the session: a successor that inherits a count past the
	// minimum would respawn at its first quiet cycle.
	next := step.State
	next.Cycles = 0
	if err := d.SaveState(next); err != nil {
		cause = fmt.Sprintf("cycle counter not saved (%v); respawning would leave the successor a stale count", err)
		witnessSessionKept(d.Out, cause)
		return unitCycleReport{SkipCause: cause}, nil
	}

	fmt.Fprintf(d.Out, "  %s %s: respawning %s\n", style.Bold.Render("🔄"), why, p.Session)
	d.RecordCycle()
	if err := d.Respawn(); err != nil {
		fmt.Fprintf(d.Out, "  %s respawn failed: %v (continuing in this session)\n", style.Error.Render("✗"), err)
		d.Escalate("witness-respawn-failed:"+p.Rig, "medium",
			fmt.Sprintf("witness %s could not respawn (%s): %v", p.Session, why, err))
		return unitCycleReport{SkipCause: "respawn failed: " + err.Error()}, nil
	}
	return unitCycleReport{Respawned: true}, nil
}

func witnessSessionKept(out io.Writer, cause string) {
	fmt.Fprintf(out, "  %s session kept: %s\n", style.Dim.Render("○"), cause)
}

// patrolCycleDir returns the directory whose .runtime holds a patrol role's
// session-cycle files (wait outcome, cycle counter, session_id), derived from
// GT_ROLE, or "" for a role that does not cycle or has cycling off. Only the
// witness cycles today; the deacon would add its own directory and flag here.
func patrolCycleDir(townRoot, gtRole string) string {
	rigName, ok := strings.CutSuffix(gtRole, "/witness")
	if !ok || rigName == "" || strings.Contains(rigName, "/") || townRoot == "" {
		return ""
	}
	if info, err := os.Stat(filepath.Join(townRoot, rigName)); err != nil || !info.IsDir() {
		return "" // never create a stray rig directory
	}
	// Flag off: write nothing, so the feature has no footprint until enabled.
	if cfg := rig.ResolveWitnessSessionConfig(townRoot, rigName); cfg == nil || !cfg.CycleSessionAtIdleCap {
		return ""
	}
	return witness.WitnessStateDir(townRoot, rigName)
}

// awaitSignalDrainNudges drains the session's queued nudges for await-signal,
// except for a role whose session cycling is enabled (patrolCycleDir != ""):
// there the next command, gt patrol report, may respawn the session, and
// nudges printed into the dying context would be lost. The report is then the
// single decision point: it drains when it keeps the session and leaves the
// queue for the successor when it respawns. Flag off: drained as before.
func awaitSignalDrainNudges(townRoot, gtRole string, drain func(string) []nudge.QueuedNudge) []nudge.QueuedNudge {
	if patrolCycleDir(townRoot, gtRole) != "" {
		return nil
	}
	return drain(townRoot)
}

// runtimeSessionID is the runtime session ID the SessionStart hook persisted
// in dir (gt prime --hook), or "" when none is recorded.
func runtimeSessionID(dir string) string {
	return readRuntimeStateFile(dir, constants.FileSessionID)
}

// recordAwaitSignalOutcome writes how this await-signal wait ended for the
// caller's patrol role, so `gt patrol report` can tell a quiet cycle from a
// busy one without trusting the agent's memory. Best-effort.
func recordAwaitSignalOutcome(townRoot string, result *AwaitSignalResult, fullTimeout, backoffMax time.Duration) {
	dir := patrolCycleDir(townRoot, os.Getenv("GT_ROLE"))
	if dir == "" {
		return
	}
	o := patrolstate.WaitOutcome{
		Reason:      result.Reason,
		Timeout:     fullTimeout,
		BackoffMax:  backoffMax,
		AtCap:       backoffMax > 0 && fullTimeout >= backoffMax,
		IdleCycles:  result.IdleCycles,
		EffortLevel: result.EffortLevel,
		SessionID:   runtimeSessionID(dir),
		At:          time.Now(),
	}
	if err := patrolstate.WriteWaitOutcome(dir, o); err != nil {
		style.PrintWarning("could not record await-signal outcome: %v", err)
	}
}

// witnessCycleParamsFor resolves the witness cycle parameters for rigName.
func witnessCycleParamsFor(townRoot, rigName string, enabled bool, minCycles, maxCycles int) witnessCycleParams {
	return witnessCycleParams{
		Rig:       rigName,
		Session:   session.WitnessSessionName(session.PrefixFor(rigName)),
		WorkDir:   witness.WitnessStateDir(townRoot, rigName),
		Enabled:   enabled,
		MinCycles: minCycles,
		MaxCycles: maxCycles,
	}
}

// defaultWitnessCycleDeps wires the real patrol report, files, tmux and
// escalation seams.
func defaultWitnessCycleDeps(roleInfo RoleInfo, p witnessCycleParams, summary, steps string, out io.Writer) witnessCycleDeps {
	return witnessCycleDeps{
		Report: func(drainNudges bool) error {
			return runPatrolReportFor(out, roleInfo, summary, steps, drainNudges)
		},
		SessionID:   func() string { return runtimeSessionID(p.WorkDir) },
		ReadWait:    func() (*patrolstate.WaitOutcome, error) { return patrolstate.ReadWaitOutcome(p.WorkDir) },
		LoadState:   func() (patrolstate.CycleState, error) { return patrolstate.LoadCycleState(p.WorkDir) },
		SaveState:   func(s patrolstate.CycleState) error { return patrolstate.SaveCycleState(p.WorkDir, s) },
		RecordCycle: func() { recordOwnSessionCycle(roleInfo.TownRoot, p.WorkDir, p.Session) },
		Respawn:     func() error { return respawnOwnSessionFresh(p.Session) },
		Escalate: func(fp, sev, msg string) {
			runBoundedEscalate("witness-session-cycle", "witness:session-cycle", fp, sev, msg)
		},
		PaneSession: func() (string, error) { return tmuxSessionForPane(os.Getenv("TMUX_PANE")) },
		HandoffAge:  func() (time.Duration, bool) { return lastHandoffAge(p.WorkDir) },
		Getenv:      os.Getenv,
		Now:         time.Now,
		Out:         out,
	}
}

// witnessRespawnEffortLine is the EFFORT directive gt prime prints for a
// witness that was just respawned by a session cycle, so its first patrol
// matches the effort the predecessor's last wait implied instead of always
// running a full patrol (claude-8w7 design, "The fresh session's first
// cycle"). Returns "" when prime has nothing to add, which means a full
// patrol.
//
// The hint is used only when wait is the outcome the respawning report
// consumed: its At equals the counter's watermark (LastWaitAt) and, when both
// know it, it came from the session the counter belonged to. Anything else (a
// newer wait nobody reported, an old record, another session's) defaults to
// a full patrol.
func witnessRespawnEffortLine(handoffReason string, wait *patrolstate.WaitOutcome, state patrolstate.CycleState) string {
	if handoffReason != unitCycleHandoffReason || wait == nil || wait.EffortLevel != "abbreviated" {
		return ""
	}
	if wait.At.IsZero() || !wait.At.Equal(state.LastWaitAt) {
		return ""
	}
	if wait.SessionID != "" && state.SessionID != "" && wait.SessionID != state.SessionID {
		return ""
	}
	return fmt.Sprintf("EFFORT: reduced (respawned at a quiet boundary; last wait: %s, idle %d). Run your first patrol ABBREVIATED.",
		wait.Reason, wait.IdleCycles)
}

// witnessPrimeEffortText reads the witness's cycle files and returns the
// respawn EFFORT line as a prime section ("" for other roles or when there
// is no hint to give).
func witnessPrimeEffortText(ctx RoleContext, handoffReason string) string {
	if ctx.Role != RoleWitness || ctx.TownRoot == "" || ctx.Rig == "" || handoffReason != unitCycleHandoffReason {
		return ""
	}
	dir := witness.WitnessStateDir(ctx.TownRoot, ctx.Rig)
	wait, err := patrolstate.ReadWaitOutcome(dir)
	if err != nil {
		return ""
	}
	state, err := patrolstate.LoadCycleState(dir)
	if err != nil {
		return ""
	}
	if line := witnessRespawnEffortLine(handoffReason, wait, state); line != "" {
		return "\n" + line + "\n"
	}
	return ""
}
