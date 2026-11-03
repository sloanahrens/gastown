package update

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Step details this version gives when it records a step without running it.
const (
	// notImplementedDetail marks the maintenance, restart and verify steps
	// recorded without running; gt-o4sk8.21 replaces them with the real steps.
	notImplementedDetail = "not implemented in this version"
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
	// Maint asks the daemon for a maintenance cycle; gt-o4sk8.21 runs it.
	Maint *MaintenanceRequest
	// NewRunID names the run.
	NewRunID func() string
	// Log receives the plan; a nil Log discards it.
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
	// maintenance waits gt-o4sk8.21 adds.
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

// runner carries one run's state through its steps. It never changes the state
// itself: every change is a Store.Mutate, so the file on disk is current after
// the first write and nothing the heartbeat writes is lost.
type runner struct {
	ctx  context.Context
	d    Deps
	o    Options
	st   State
	owns bool // the hold this run created, and so may release
}

// walk runs the steps from index start in plan order and reports how the run
// ended. A returned error is a record that could not be written.
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
		case StepMaintenance, StepRestart, StepVerify:
			_, err = r.mutate(func(st *State) {
				st.Steps[i].Result = ResultSkipped
				st.Steps[i].Detail = notImplementedDetail
			})
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
		if outcome != "" {
			return outcome, nil
		}
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
