package update

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Step details for a step that ends with less than the run set out to do.
const (
	// restartPendingDetail is the restart step's note when the daemon never
	// reported the new build inside the wait: the new binary is installed and
	// the town keeps running on the old one until the daemon restarts.
	restartPendingDetail = "installed, restart pending"
	// verifyRestartPendingDetail is why verify is skipped after that timeout:
	// the daemon still answers from the old build, which is the failure the
	// restart step already reported.
	verifyRestartPendingDetail = "restart pending"
	// noHoldDetail marks a lift step with no hold the run created.
	noHoldDetail = "hold not set by this run"
)

// Deps is the effects one update run reaches.
type Deps struct {
	// Clock stamps every time the run records.
	Clock Clock
	// Sleep waits between polls and returns the context's error, so a wait
	// ends as soon as the run is canceled.
	Sleep func(context.Context, time.Duration) error
	// Store holds the run's record.
	Store *Store
	// Probes answers the plan's questions about the town.
	Probes Probes
	// Seats lists the seats the drain waits for.
	Seats SeatProbe
	// Hold is the dispatch hold the run takes before it drains and installs.
	Hold *Hold
	// Installer puts the target build in force.
	Installer Installer
	// Maint asks the daemon for a maintenance cycle and waits for its result.
	Maint *MaintenanceRequest
	// Verify reports what the town is running once the restart is done.
	Verify VerifyProbes
	// NewRunID names the run.
	NewRunID func() string
	// Log receives the plan and each step's own note; a nil Log discards it.
	Log io.Writer
}

// Options narrows what a run does and how long its waits may last.
type Options struct {
	// DryRun prints the plan and stops before anything is written or done.
	DryRun bool
	// Skip and Only select steps, as BuildPlan reads them.
	Skip, Only map[StepName]bool
	// Force plans an install even when the installed build is main.
	Force bool
	// Source is the record's provenance: SourceManual (the default) or
	// SourceDaemon.
	Source string
	// Drain bounds the wait for working seats.
	Drain DrainOptions
	// RestartWait, RestartPoll, MaintWait and MaintPoll bound the restart and
	// maintenance waits.
	RestartWait, RestartPoll, MaintWait, MaintPoll time.Duration
}

// Run plans one update, records the plan and every step's progress, and
// returns the record as the run ended. A dry run returns the state a real run
// would start with and writes nothing. The error is a failed or invalid plan,
// or a record that could not be written.
func Run(ctx context.Context, d Deps, o Options) (State, error) {
	steps, target, err := BuildPlan(ctx, d.Probes, PlanOptions{Skip: o.Skip, Only: o.Only, Force: o.Force})
	if err != nil {
		return State{}, err
	}
	st := newRunState(d, o, steps, target)
	writePlan(d.Log, st.Steps)
	if o.DryRun {
		st.StartedAt, st.HeartbeatAt = time.Time{}, time.Time{}
		st.Outcome = OutcomeOK
		return st, nil
	}
	// The first write publishes the whole plan; every change after it is a
	// Mutate, which serializes with the command's heartbeat timer.
	if err := d.Store.Write(st); err != nil {
		return State{}, err
	}
	r := &runner{ctx: ctx, d: d, o: o, st: st}
	outcome, err := r.walk(0)
	if err != nil {
		return r.st, err
	}
	final, err := r.finish(outcome)
	if err != nil {
		return r.st, err
	}
	// History is how a later run and the daemon see how this one ended; a write
	// that fails is reported in the log, because the run itself is finished and
	// its state.json is the record of it.
	if err := d.Store.AppendHistory(final); err != nil {
		logf(d.Log, "append update history: %v\n", err)
	}
	return final, nil
}

// newRunState is the record a run starts with, before anything has happened.
func newRunState(d Deps, o Options, steps []StepRecord, target Target) State {
	source := o.Source
	if source == "" {
		source = SourceManual
	}
	now := d.Clock.Now()
	planned := make([]StepRecord, len(steps))
	copy(planned, steps)
	for i := range planned {
		planned[i].Result = ResultPending
	}
	return State{
		V:           stateVersion,
		RunID:       d.NewRunID(),
		Source:      source,
		StartedAt:   now,
		HeartbeatAt: now,
		Target:      target,
		Steps:       planned,
		Outcome:     OutcomeRunning,
	}
}

// writePlan prints each step's decision and the reason the plan gave it.
func writePlan(w io.Writer, steps []StepRecord) {
	if w == nil {
		return
	}
	for _, step := range steps {
		action := "skip"
		if step.WillRun {
			action = "run"
		}
		fmt.Fprintf(w, "%s: %s: %s\n", step.Name, action, step.Reason)
	}
}

// logf writes one line for a person reading the run's log; a nil writer
// discards it.
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format, args...)
}

// runner carries one run's state through its steps. It never changes the state
// itself: every change is a Store.Mutate, so the file on disk is current after
// the first write and nothing the heartbeat writes is lost.
type runner struct {
	ctx  context.Context
	d    Deps
	o    Options
	st   State
	owns bool // the hold this run created, and so may release
	// partial records that a step fell short without ending the run. The run
	// ends partial unless a later step ends it worse.
	partial bool
	// restartPending records that the restart timed out, so the walk leaves the
	// verify record the restart step wrote in place.
	restartPending bool
}

// walk runs the steps from index start in plan order and reports how the run
// ended. A step that ends the run decides its outcome here; a step that falls
// short without ending it marks the run partial and the walk goes on. A
// returned error is a record that could not be written.
func (r *runner) walk(start int) (Outcome, error) {
	for i := start; i < len(r.st.Steps); i++ {
		if r.ctx.Err() != nil {
			return OutcomeInterrupted, nil
		}
		step := r.st.Steps[i]
		if !step.WillRun {
			if _, err := r.mutate(func(st *State) {
				st.Steps[i].Result = ResultSkipped
				if st.Steps[i].Name == StepLift {
					st.Steps[i].Detail = noHoldDetail
				}
			}); err != nil {
				return "", err
			}
			continue
		}
		var (
			outcome Outcome
			err     error
		)
		switch step.Name {
		case StepDrain:
			return r.drainHeld(i)
		case StepMaintenance:
			outcome, err = r.maintenance(i)
		case StepRestart:
			outcome, err = r.restart(i)
		case StepVerify:
			if r.restartPending {
				// The restart step recorded this one as skipped.
				continue
			}
			outcome, err = r.verify(i)
		case StepInstall:
			outcome, err = r.install(i)
		case StepLift:
			outcome, err = r.lift(i)
		default:
			err = fmt.Errorf("update step %q is not a step this run knows", step.Name)
		}
		if err != nil {
			return "", err
		}
		if outcome == OutcomePartial {
			r.partial = true
			continue
		}
		if outcome != "" {
			return outcome, nil
		}
	}
	if r.partial {
		return OutcomePartial, nil
	}
	return OutcomeOK, nil
}

// drainHeld takes the hold and runs the drain and every later step inside it,
// so the hold is released on every way out of the walk, a cancel included.
func (r *runner) drainHeld(i int) (Outcome, error) {
	note := fmt.Sprintf("gt update %s: draining", r.st.RunID)
	var (
		outcome Outcome
		walkErr error
	)
	// The callback returns walkErr only for a record that could not be
	// written; a decided outcome is carried in outcome and ends the walk
	// normally, so WithHold treats it as a finished walk.
	held := false
	err := WithHold(r.ctx, r.d.Hold, note, func(rec HoldRecord) error {
		held = true
		r.owns = rec.Owned
		if _, err := r.mutate(func(st *State) { st.Hold = rec }); err != nil {
			walkErr = err
			return err
		}
		if outcome, walkErr = r.drain(i); walkErr != nil {
			return walkErr
		}
		if outcome == "" {
			outcome, walkErr = r.walk(i + 1)
		}
		return walkErr
	})
	if walkErr != nil {
		return "", walkErr
	}
	if err != nil {
		if !held {
			// The hold could not be taken. Draining into an unknown hold and
			// installing without one is exactly what the hold prevents.
			return r.holdFailed(i, err)
		}
		return r.releaseFailed(err)
	}
	if r.owns {
		// The walk ended before the lift step; WithHold released the hold it
		// owns, so the record must no longer say the run holds it.
		r.owns = false
		if _, err := r.mutate(func(st *State) { st.Hold.Owned = false }); err != nil {
			return "", err
		}
	}
	return outcome, nil
}

// holdFailed records a hold that could not be taken on the drain step and ends
// the run aborted, with nothing drained and nothing installed: an unreadable
// hold is never read as no hold.
func (r *runner) holdFailed(i int, err error) (Outcome, error) {
	if _, merr := r.mutate(func(st *State) {
		st.Steps[i].Result = ResultFailed
		st.Steps[i].Detail = "acquire the dispatch hold: " + err.Error()
	}); merr != nil {
		return "", merr
	}
	return OutcomeAborted, nil
}

// releaseFailed records a hold the run could not release and ends the run
// failed: dispatch stays stopped until someone removes the file by hand, which
// must not read as a clean ending.
func (r *runner) releaseFailed(err error) (Outcome, error) {
	if _, merr := r.mutate(func(st *State) {
		for i := range st.Steps {
			if st.Steps[i].Name != StepLift {
				continue
			}
			st.Steps[i].Result = ResultFailed
			st.Steps[i].EndedAt = r.d.Clock.Now()
			st.Steps[i].Detail = "release the dispatch hold: " + err.Error()
		}
	}); merr != nil {
		return "", merr
	}
	return OutcomeFailed, nil
}

// drain runs the drain step. A drain that cannot finish ends the run aborted,
// with the seats still working or the seat probe's error in the step.
func (r *runner) drain(i int) (Outcome, error) {
	if err := r.beginStep(i); err != nil {
		return "", err
	}
	res, err := Drain(r.ctx, r.d.Clock, r.d.Sleep, r.d.Seats, r.o.Drain)
	if r.ctx.Err() != nil {
		return r.failStep(i, "canceled", OutcomeInterrupted)
	}
	if err != nil {
		return r.failStep(i, err.Error(), OutcomeFailed)
	}
	if !res.Drained {
		return r.failStep(i, drainFailureDetail(res), OutcomeAborted)
	}
	if err := r.endStep(i, ResultOK, ""); err != nil {
		return "", err
	}
	return "", nil
}

// drainFailureDetail says why the drain did not finish: the probe's error when
// it was still failing at the timeout, otherwise the seats still working.
func drainFailureDetail(res DrainResult) string {
	if res.Err != nil {
		return fmt.Sprintf("seat probe failed: %v", res.Err)
	}
	return "seats still working: " + strings.Join(res.Blocking, ", ")
}

// install runs the install step. A busy installer aborts the run with nothing
// changed; a failed one ends it failed; the hold is released either way.
func (r *runner) install(i int) (Outcome, error) {
	if err := r.beginStep(i); err != nil {
		return "", err
	}
	res, err := r.d.Installer.Install(r.ctx, r.st.Target.SHA)
	if r.ctx.Err() != nil {
		return r.failStep(i, "canceled", OutcomeInterrupted)
	}
	result := ClassifyInstall(res)
	if err != nil {
		result = ResultFailed
	}
	if err := r.endStep(i, result, installDetail(res, result, err)); err != nil {
		return "", err
	}
	switch result {
	case ResultOK:
		return "", nil
	case ResultDeferred:
		return OutcomeAborted, nil
	default:
		return OutcomeFailed, nil
	}
}

// installDetail is the step's detail: the installer's own reason, or why its
// silence leaves the run short of a success.
func installDetail(res InstallResult, result StepResult, err error) string {
	if res.Reason != "" {
		return res.Reason
	}
	switch {
	case err != nil:
		return err.Error()
	case result == ResultDeferred:
		return "the installer is busy"
	case result != ResultOK:
		return "the installer failed"
	}
	return ""
}

// maintenance asks the daemon for one maintenance cycle and waits for it. A
// deferred or failed cycle does not stop the run: the install goes ahead and
// the run ends partial, because the town is not in the state the run set out
// to leave it in.
func (r *runner) maintenance(i int) (Outcome, error) {
	if err := r.beginStep(i); err != nil {
		return "", err
	}
	res, err := r.maintenanceCycle()
	if r.ctx.Err() != nil {
		return r.failStep(i, "canceled", OutcomeInterrupted)
	}
	result := maintenanceStepResult(res, err)
	detail := maintenanceDetail(res, err)
	if detail != "" {
		logf(r.d.Log, "maintenance: %s: %s\n", result, detail)
	}
	if err := r.endStep(i, result, detail); err != nil {
		return "", err
	}
	if result != ResultOK {
		return OutcomePartial, nil
	}
	return "", nil
}

// maintenanceCycle writes the run's request and waits for the daemon's answer.
func (r *runner) maintenanceCycle() (MaintenanceResult, error) {
	if err := r.d.Maint.Request(r.st.RunID); err != nil {
		return MaintenanceResult{}, err
	}
	return r.d.Maint.Wait(r.ctx, r.d.Sleep, r.st.RunID, r.o.MaintWait, r.o.MaintPoll)
}

// maintenanceStepResult maps the cycle onto the step's result. An outcome the
// request protocol does not define is a failure, never a success.
func maintenanceStepResult(res MaintenanceResult, err error) StepResult {
	switch {
	case err != nil:
		return ResultFailed
	case res.Outcome == "completed":
		return ResultOK
	case res.Outcome == maintenanceDeferred:
		return ResultDeferred
	default:
		return ResultFailed
	}
}

// maintenanceDetail is the cycle's own note, or the error that ended it.
func maintenanceDetail(res MaintenanceResult, err error) string {
	if err != nil {
		return err.Error()
	}
	return res.Detail
}

// restart waits for the daemon to come up on the new build. A timeout leaves
// the install in place and makes the run partial: the town keeps working on the
// old binary until the daemon restarts, so verify cannot pass and is recorded
// as skipped here.
func (r *runner) restart(i int) (Outcome, error) {
	if err := r.beginStep(i); err != nil {
		return "", err
	}
	restarted, err := WaitForRestart(r.ctx, r.d.Clock, r.d.Sleep, r.d.Verify, r.st.Target.SHA, r.o.RestartWait, r.o.RestartPoll)
	if r.ctx.Err() != nil {
		return r.failStep(i, "canceled", OutcomeInterrupted)
	}
	if err != nil {
		// WaitForRestart reports the sleep's error, which is the context's, so
		// the wait could not be completed and the run is cut short.
		return r.failStep(i, err.Error(), OutcomeInterrupted)
	}
	if restarted {
		if err := r.endStep(i, ResultOK, ""); err != nil {
			return "", err
		}
		return "", nil
	}
	if err := r.endStep(i, ResultDeferred, restartPendingDetail); err != nil {
		return "", err
	}
	if _, err := r.mutate(func(st *State) {
		for j := range st.Steps {
			if st.Steps[j].Name == StepVerify {
				st.Steps[j].Result = ResultSkipped
				st.Steps[j].Detail = verifyRestartPendingDetail
			}
		}
	}); err != nil {
		return "", err
	}
	r.restartPending = true
	return OutcomePartial, nil
}

// verify checks every probe against the target build. A check that fails makes
// the run partial: the install happened, so the town is not untouched, but it
// is not on the build the run installed.
func (r *runner) verify(i int) (Outcome, error) {
	if err := r.beginStep(i); err != nil {
		return "", err
	}
	checks := Verify(r.ctx, r.d.Verify, r.st.Target.SHA)
	if r.ctx.Err() != nil {
		return r.failStep(i, "canceled", OutcomeInterrupted)
	}
	failed := failedChecks(checks)
	if len(failed) == 0 {
		if err := r.endStep(i, ResultOK, ""); err != nil {
			return "", err
		}
		return "", nil
	}
	if err := r.endStep(i, ResultFailed, checksDetail(failed)); err != nil {
		return "", err
	}
	return OutcomePartial, nil
}

// failedChecks returns the checks that did not match the target build.
func failedChecks(checks []Check) []Check {
	var failed []Check
	for _, c := range checks {
		if !c.OK {
			failed = append(failed, c)
		}
	}
	return failed
}

// checksDetail names each failed check and what it reported, so the record says
// which part of the town is not on the new build.
func checksDetail(checks []Check) string {
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = c.Name + ": " + c.Detail
	}
	return strings.Join(parts, "; ")
}

// lift releases the hold when this run owns it and records the release. A hold
// the run did not create is left in place.
func (r *runner) lift(i int) (Outcome, error) {
	if !r.owns {
		if _, err := r.mutate(func(st *State) {
			st.Steps[i].Result = ResultSkipped
			st.Steps[i].Detail = noHoldDetail
		}); err != nil {
			return "", err
		}
		return "", nil
	}
	if err := r.beginStep(i); err != nil {
		return "", err
	}
	if err := r.d.Hold.Release(); err != nil {
		if e := r.endStep(i, ResultFailed, err.Error()); e != nil {
			return "", e
		}
		return OutcomeFailed, nil
	}
	r.owns = false
	if _, err := r.mutate(func(st *State) {
		st.Steps[i].Result = ResultOK
		st.Steps[i].EndedAt = r.d.Clock.Now()
		st.Hold.Owned = false
	}); err != nil {
		return "", err
	}
	return "", nil
}

// beginStep records that step i is running now.
func (r *runner) beginStep(i int) error {
	_, err := r.mutate(func(st *State) {
		st.Steps[i].Result = ResultRunning
		st.Steps[i].StartedAt = r.d.Clock.Now()
		st.Current = st.Steps[i].Name
	})
	return err
}

// endStep records when step i ended and how.
func (r *runner) endStep(i int, result StepResult, detail string) error {
	_, err := r.mutate(func(st *State) {
		st.Steps[i].Result = result
		st.Steps[i].EndedAt = r.d.Clock.Now()
		st.Steps[i].Detail = detail
		st.Current = ""
	})
	return err
}

// failStep ends step i failed with detail and reports the outcome the step
// decided.
func (r *runner) failStep(i int, detail string, outcome Outcome) (Outcome, error) {
	if err := r.endStep(i, ResultFailed, detail); err != nil {
		return "", err
	}
	return outcome, nil
}

// finish records how the run ended and returns the final record.
func (r *runner) finish(outcome Outcome) (State, error) {
	return r.mutate(func(st *State) {
		st.Outcome = outcome
		st.EndedAt = r.d.Clock.Now()
		st.Current = ""
	})
}

// mutate applies fn through the store and adopts the record it published.
func (r *runner) mutate(fn func(*State)) (State, error) {
	st, err := r.d.Store.Mutate(fn)
	if err != nil {
		return State{}, err
	}
	r.st = st
	return st, nil
}
