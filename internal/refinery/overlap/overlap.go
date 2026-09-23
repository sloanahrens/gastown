// Package overlap runs the single-MR refinery path's test suite and om
// editorial review concurrently, in one Go process, instead of serially: the
// two used to run one after the other (mol-refinery-patrol's run-tests then
// quality-review steps), which spent the review's wall time (p50 4.5 min)
// entirely on top of the suite's (p50 4.4 min) even though nothing about the
// review depends on the suite finishing first.
//
// Join starts both under one cancellable context and blocks until both
// return — a bounded wait, not a poll — so a merge can never see a
// still-running review: its own goroutine has not returned, so Join has not
// returned, so no caller has read a verdict yet. There is no on-disk state:
// both results travel back as Go values on the same call stack that started
// them, tagged with LaunchID and HeadSHA so a caller (and a test) can bind a
// result to the invocation that produced it without a file for a stale
// attempt to leave behind.
package overlap

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// SuiteStep is one named shell command run in order — setup, typecheck,
// lint, build, test, matching mol-refinery-patrol's run-tests step. An empty
// Cmd means "not configured for this project" and is skipped silently, the
// same rule the formula documents today.
type SuiteStep struct {
	Name string
	Cmd  string
}

// StepResult is the outcome of one SuiteStep.
type StepResult struct {
	Name     string
	Success  bool
	ExitCode int
	Output   string
	Elapsed  time.Duration
}

// SuiteResult is the outcome of running every configured SuiteStep in order,
// stopping at the first failure — the same stop-on-first-failure rule
// runGatesForPhase's sequential mode uses.
type SuiteResult struct {
	// Ran is false when every step was empty (run_tests=false, or nothing
	// configured), matching runVerification's "nothing configured" no-op.
	Ran        bool
	Success    bool
	FailedStep string
	Steps      []StepResult
}

// SuiteRunner executes one SuiteStep under ctx and returns its outcome.
// Production runs the command through a shell, killed by process group on
// ctx cancellation (see RealRunStep); tests inject a stub so the join logic
// below never shells out.
type SuiteRunner func(ctx context.Context, step SuiteStep) StepResult

// RunSuite runs steps in order, skipping empty commands, stopping at the
// first failure (including one caused by ctx already being done — a step
// started after the overlap's timeout cancels must not report a false
// pass).
func RunSuite(ctx context.Context, steps []SuiteStep, run SuiteRunner) SuiteResult {
	result := SuiteResult{Success: true}
	for _, step := range steps {
		if strings.TrimSpace(step.Cmd) == "" {
			continue
		}
		result.Ran = true
		if err := ctx.Err(); err != nil {
			result.Success = false
			result.FailedStep = step.Name
			result.Steps = append(result.Steps, StepResult{
				Name:    step.Name,
				Success: false,
				Output:  err.Error(),
			})
			return result
		}
		sr := run(ctx, step)
		sr.Name = step.Name
		result.Steps = append(result.Steps, sr)
		if !sr.Success {
			result.Success = false
			result.FailedStep = step.Name
			return result
		}
	}
	return result
}

// ReviewRunner runs the om editorial review under ctx and returns its
// verdict. Production wraps editorial.Run (see Request.Review doc); tests
// inject a stub returning a canned ReviewResult, so the join logic below
// never invokes om or touches git.
type ReviewRunner func(ctx context.Context) editorial.ReviewResult

// Timer is the subset of *time.Timer Join needs, so a test can drive the
// overlap's timeout deterministically — firing it, or not, on its own
// schedule — without a real sleep. See Clock.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock builds the Timer Join bounds itself with. RealClock is production;
// tests substitute a fake that hands back a channel they control, which is
// what makes the timeout-cancellation tests deterministic instead of racing
// a real deadline.
type Clock interface {
	NewTimer(d time.Duration) Timer
}

// RealClock is the production Clock, backed by time.Timer.
type RealClock struct{}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// NewTimer implements Clock.
func (RealClock) NewTimer(d time.Duration) Timer { return realTimer{t: time.NewTimer(d)} }

// Request is one overlap invocation.
type Request struct {
	// HeadSHA is the already-resolved commit the suite runs against and the
	// review is bound to — never a moving ref (the gt-dcku/gt-kmul hazard
	// this design avoids by construction: the review takes the no-checkout
	// path in editorial.Run whenever RehearsedHead is set, so it never
	// touches the live clone's HEAD or index while the suite runs there
	// concurrently).
	HeadSHA string

	// Suite is the ordered quality-check/test steps to run. An empty slice
	// means nothing configured — SuiteResult.Ran is false and Success is
	// true, the same "pass by default" rule runVerification uses.
	Suite []SuiteStep

	// Review runs the om editorial review. Nil when the rig has not set
	// merge_queue.editorial.required (mirrors quality-review's Step 0 skip)
	// — Join then does not start a review goroutine at all, and Result.Review
	// is nil.
	Review ReviewRunner

	// Timeout bounds the whole join: when it elapses before both sides have
	// returned, Join cancels ctx (killing the suite step's and the review's
	// subprocess by process group — see RealRunStep and
	// editorial.RunGateScript) and, once both goroutines have actually
	// exited, returns with Result.TimedOut set and reviewed the same way
	// every other unattributable review failure is: Exit mapped to 2, never
	// an approval.
	Timeout time.Duration
}

// Deps are Join's collaborators, all substitutable for testing.
type Deps struct {
	RunStep     SuiteRunner
	Clock       Clock
	NewLaunchID func() string
}

// Result is the joined outcome of one overlap invocation, keyed by LaunchID
// and HeadSHA so a caller can confirm a result belongs to the call that
// produced it rather than some other invocation.
type Result struct {
	LaunchID string
	HeadSHA  string
	Suite    SuiteResult
	// Review is nil exactly when Request.Review was nil — no review ran.
	Review   *editorial.ReviewResult
	TimedOut bool
}

// Join runs the suite and, when configured, the om review concurrently
// under one cancellable context, and blocks until both have returned. It
// never polls and never touches disk: both results travel back as the
// return value of this one call.
func Join(ctx context.Context, req Request, deps Deps) Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	launchID := deps.NewLaunchID()
	timer := deps.Clock.NewTimer(req.Timeout)
	defer timer.Stop()

	var wg sync.WaitGroup
	var suiteResult SuiteResult
	var reviewResult editorial.ReviewResult
	reviewRan := req.Review != nil

	wg.Add(1)
	go func() {
		defer wg.Done()
		suiteResult = RunSuite(ctx, req.Suite, deps.RunStep)
	}()

	if reviewRan {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reviewResult = req.Review(ctx)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timedOut := false
	select {
	case <-done:
	case <-timer.C():
		timedOut = true
		// Cancel first, then block on <-done: Join must never return while
		// either goroutine is still running in the background — that would
		// be exactly the orphaned-process/leaked-goroutine hazard the prior
		// shell/PID-file attempts at this design were rejected for.
		cancel()
		<-done
	}

	if timedOut && reviewRan {
		reviewResult = classifyTimedOutReview(reviewResult)
	}

	result := Result{
		LaunchID: launchID,
		HeadSHA:  req.HeadSHA,
		Suite:    suiteResult,
		TimedOut: timedOut,
	}
	if reviewRan {
		rr := reviewResult
		result.Review = &rr
	}
	return result
}

// classifyTimedOutReview maps a review that never reached a verdict before
// an overlap timeout canceled it to a synthesized exit 2 — the same
// fail-closed rule editorial.Run applies to every other infra failure, so an
// empty exit can never read as an approval. A review that already recorded
// a real verdict (Note != nil) before the timeout fired is returned
// unchanged: the verdict stands even though the slower suite is what
// actually tripped the bound.
func classifyTimedOutReview(r editorial.ReviewResult) editorial.ReviewResult {
	if r.Exit == 0 && r.Note == nil {
		r.Exit = 2
		r.Class = editorial.BackendTimeout
		r.Stderr = "om editorial review canceled: overlap timeout"
	}
	return r
}

// Action is the merge decision Decide derives from a joined Result.
type Action string

const (
	// ActionMerge means both gates cleared: the suite passed, and either no
	// review was required or the review approved. Proceed to merge-push.
	ActionMerge Action = "merge"
	// ActionRejectSuite means the suite failed. Any review verdict is
	// discarded, never acted on: an approve verdict never substitutes for a
	// red suite, and a request_changes or exit-2 review verdict never sends
	// a second FIX_NEEDED alongside the suite's own.
	ActionRejectSuite Action = "reject_suite"
	// ActionRejectReview means the suite passed but the review requested
	// changes (exit 1).
	ActionRejectReview Action = "reject_review"
	// ActionEscalateReview means the suite passed but the review could not
	// produce a verdict (exit 2, including an overlap timeout).
	ActionEscalateReview Action = "escalate_review"
)

// Decision is Decide's classification of a joined Result.
type Decision struct {
	Action Action
	// ReviewDiscarded is true when a review verdict exists but Action is
	// ActionRejectSuite — the verdict was computed but must not be acted on.
	ReviewDiscarded bool
}

// Decide applies the join semantics: both gates stay mandatory, and a
// failed suite always wins over whatever the review decided, so a race
// between the two can never let a merge through on a partial result.
func Decide(result Result) Decision {
	if !result.Suite.Success {
		return Decision{Action: ActionRejectSuite, ReviewDiscarded: result.Review != nil}
	}
	if result.Review == nil {
		return Decision{Action: ActionMerge}
	}
	switch result.Review.Exit {
	case 0:
		return Decision{Action: ActionMerge}
	case 1:
		return Decision{Action: ActionRejectReview}
	default:
		return Decision{Action: ActionEscalateReview}
	}
}
