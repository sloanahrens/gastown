package overlap

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// fakeTimer is a Timer whose channel a test fires by hand, so Join's
// timeout path is exercised without a real sleep (mayor's requirement: fake
// clock, no wall-clock sleeps, no timing assertions).
type fakeTimer struct {
	c       chan time.Time
	stopped atomic.Bool
}

func newFakeTimer() *fakeTimer { return &fakeTimer{c: make(chan time.Time, 1)} }

func (f *fakeTimer) C() <-chan time.Time { return f.c }
func (f *fakeTimer) Stop() bool          { f.stopped.Store(true); return true }
func (f *fakeTimer) fire()               { f.c <- time.Time{} }

// fakeClock hands out a single fakeTimer per NewTimer call, keeping the
// most recent one reachable so the test can fire it. A never-firing clock
// (fire=false) proves the "both finish before the bound" path never touches
// the timeout branch at all.
type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := newFakeTimer()
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) last() *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timers[len(c.timers)-1]
}

func launchIDSeq() func() string {
	var n atomic.Int64
	return func() string { return fmt.Sprintf("launch-%d", n.Add(1)) }
}

// --- Required test matrix (gt-cmcv.1): approve+red, reject+green, both green ---

func TestDecide_ApproveAndRed_RejectsViaSuite_DiscardsReview(t *testing.T) {
	result := Result{
		Suite:  SuiteResult{Ran: true, Success: false, FailedStep: "test"},
		Review: &editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 0.9}},
	}
	got := Decide(result)
	if got.Action != ActionRejectSuite {
		t.Fatalf("Action = %v, want ActionRejectSuite", got.Action)
	}
	if !got.ReviewDiscarded {
		t.Fatalf("ReviewDiscarded = false, want true — an approve verdict must never paper over a red suite")
	}
}

func TestDecide_RejectAndGreen_RejectsViaReview(t *testing.T) {
	result := Result{
		Suite:  SuiteResult{Ran: true, Success: true},
		Review: &editorial.ReviewResult{Exit: 1, Note: &editorial.Note{Score: 0.3}},
	}
	got := Decide(result)
	if got.Action != ActionRejectReview {
		t.Fatalf("Action = %v, want ActionRejectReview", got.Action)
	}
	if got.ReviewDiscarded {
		t.Fatalf("ReviewDiscarded = true, want false — a green suite must act on the review verdict")
	}
}

func TestDecide_BothGreen_Merges(t *testing.T) {
	result := Result{
		Suite:  SuiteResult{Ran: true, Success: true},
		Review: &editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 0.95}},
	}
	got := Decide(result)
	if got.Action != ActionMerge {
		t.Fatalf("Action = %v, want ActionMerge", got.Action)
	}
}

func TestDecide_NoReviewConfigured_GreenSuiteMerges(t *testing.T) {
	result := Result{Suite: SuiteResult{Ran: true, Success: true}, Review: nil}
	got := Decide(result)
	if got.Action != ActionMerge {
		t.Fatalf("Action = %v, want ActionMerge", got.Action)
	}
}

func TestDecide_ReviewExit2_Escalates(t *testing.T) {
	result := Result{
		Suite:  SuiteResult{Ran: true, Success: true},
		Review: &editorial.ReviewResult{Exit: 2, Class: editorial.BackendTimeout},
	}
	got := Decide(result)
	if got.Action != ActionEscalateReview {
		t.Fatalf("Action = %v, want ActionEscalateReview", got.Action)
	}
	if got.ReviewDiscarded {
		t.Fatalf("ReviewDiscarded = true, want false — exit 2 on a green suite is escalated, not silently dropped")
	}
}

// --- Join: concurrency, in-process keying, timeout/cancellation ---

func TestJoin_RunsSuiteAndReviewConcurrently(t *testing.T) {
	suiteRelease := make(chan struct{})
	reviewRelease := make(chan struct{})
	started := make(chan string, 2)

	runStep := func(ctx context.Context, step SuiteStep) StepResult {
		started <- "suite"
		<-suiteRelease
		return StepResult{Success: true}
	}
	review := func(ctx context.Context) editorial.ReviewResult {
		started <- "review"
		<-reviewRelease
		return editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 1}}
	}

	deps := Deps{RunStep: runStep, Clock: &fakeClock{}, NewLaunchID: launchIDSeq()}
	req := Request{
		HeadSHA: "deadbeef",
		Suite:   []SuiteStep{{Name: "test", Cmd: "make test"}},
		Review:  review,
		Timeout: time.Hour,
	}

	resultCh := make(chan Result, 1)
	go func() { resultCh <- Join(context.Background(), req, deps) }()

	// Both goroutines must have started before either is released — proves
	// the suite and the review run concurrently, not one after the other.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for both goroutines to start; seen=%v", seen)
		}
	}
	if !seen["suite"] || !seen["review"] {
		t.Fatalf("expected both suite and review to start, seen=%v", seen)
	}

	close(suiteRelease)
	close(reviewRelease)

	select {
	case result := <-resultCh:
		if !result.Suite.Success {
			t.Fatalf("Suite.Success = false, want true")
		}
		if result.Review == nil || result.Review.Exit != 0 {
			t.Fatalf("Review = %+v, want Exit 0", result.Review)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Join did not return after both sides were released")
	}
}

func TestJoin_LaunchIDAndHeadSHA_BindEachResult(t *testing.T) {
	deps := Deps{
		RunStep:     func(ctx context.Context, step SuiteStep) StepResult { return StepResult{Success: true} },
		Clock:       &fakeClock{},
		NewLaunchID: launchIDSeq(),
	}
	req1 := Request{HeadSHA: "sha-one", Suite: []SuiteStep{{Name: "test", Cmd: "make test"}}, Timeout: time.Hour}
	req2 := Request{HeadSHA: "sha-two", Suite: []SuiteStep{{Name: "test", Cmd: "make test"}}, Timeout: time.Hour}

	r1 := Join(context.Background(), req1, deps)
	r2 := Join(context.Background(), req2, deps)

	if r1.HeadSHA != "sha-one" || r2.HeadSHA != "sha-two" {
		t.Fatalf("HeadSHA not bound correctly: r1=%q r2=%q", r1.HeadSHA, r2.HeadSHA)
	}
	if r1.LaunchID == "" || r2.LaunchID == "" || r1.LaunchID == r2.LaunchID {
		t.Fatalf("expected distinct non-empty LaunchIDs, got r1=%q r2=%q", r1.LaunchID, r2.LaunchID)
	}
}

func TestJoin_NoReviewConfigured_OnlyRunsSuite(t *testing.T) {
	reviewCalled := false
	deps := Deps{
		RunStep:     func(ctx context.Context, step SuiteStep) StepResult { return StepResult{Success: true} },
		Clock:       &fakeClock{},
		NewLaunchID: launchIDSeq(),
	}
	req := Request{
		HeadSHA: "sha",
		Suite:   []SuiteStep{{Name: "test", Cmd: "make test"}},
		Review:  nil,
		Timeout: time.Hour,
	}
	result := Join(context.Background(), req, deps)
	if result.Review != nil {
		t.Fatalf("Review = %+v, want nil when Request.Review is nil", result.Review)
	}
	if reviewCalled {
		t.Fatal("review runner invoked despite Request.Review being nil")
	}
}

// TestJoin_TimeoutCancelsBoth_MapsEmptyReviewToExit2 is the required
// timeout/cancellation case: the suite is still running (never finishes on
// its own in this test) and the review has not yet produced a verdict when
// the fake clock fires. Join must cancel both, wait for both goroutines to
// actually return (no orphaned goroutine), and map the review's empty exit
// to 2 — never an approval.
func TestJoin_TimeoutCancelsBoth_MapsEmptyReviewToExit2(t *testing.T) {
	suiteStarted := make(chan struct{})
	reviewStarted := make(chan struct{})
	var suiteCanceled, reviewCanceled atomic.Bool

	runStep := func(ctx context.Context, step SuiteStep) StepResult {
		close(suiteStarted)
		<-ctx.Done()
		suiteCanceled.Store(true)
		return StepResult{Success: false, Output: "canceled"}
	}
	review := func(ctx context.Context) editorial.ReviewResult {
		close(reviewStarted)
		<-ctx.Done()
		reviewCanceled.Store(true)
		return editorial.ReviewResult{} // no verdict reached
	}

	clock := &fakeClock{}
	deps := Deps{RunStep: runStep, Clock: clock, NewLaunchID: launchIDSeq()}
	req := Request{
		HeadSHA: "sha-timeout",
		Suite:   []SuiteStep{{Name: "test", Cmd: "make test"}},
		Review:  review,
		Timeout: time.Hour, // irrelevant: the fake timer is fired by hand below
	}

	resultCh := make(chan Result, 1)
	go func() { resultCh <- Join(context.Background(), req, deps) }()

	<-suiteStarted
	<-reviewStarted
	clock.last().fire()

	select {
	case result := <-resultCh:
		if !result.TimedOut {
			t.Fatal("TimedOut = false, want true")
		}
		if result.Suite.Success {
			t.Fatal("Suite.Success = true after a canceled step, want false")
		}
		if result.Review == nil {
			t.Fatal("Review = nil, want a synthesized exit-2 result")
		}
		if result.Review.Exit != 2 {
			t.Fatalf("Review.Exit = %d, want 2 (empty/canceled review must never read as approve)", result.Review.Exit)
		}
		if result.Review.Class != editorial.BackendTimeout {
			t.Fatalf("Review.Class = %q, want %q", result.Review.Class, editorial.BackendTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Join did not return after the timer fired — it must not wait past both goroutines actually exiting")
	}

	if !suiteCanceled.Load() || !reviewCanceled.Load() {
		t.Fatalf("expected both sides to observe cancellation: suite=%v review=%v", suiteCanceled.Load(), reviewCanceled.Load())
	}
}

// TestClassifyTimedOutReview_PreservesAnAlreadyRecordedVerdict covers the
// case where the review finished with a real verdict before the slower
// suite trips the overlap timeout: the already-recorded verdict must
// survive, not be clobbered by the empty-exit synthesis (which exists only
// for a review that never reached a verdict). Unit-tested directly — not
// through Join's goroutines — because whether the review happens to finish
// before the timer fires is exactly the kind of scheduling race a
// wall-clock-free test must not depend on.
func TestClassifyTimedOutReview_PreservesAnAlreadyRecordedVerdict(t *testing.T) {
	recorded := editorial.ReviewResult{Exit: 1, Note: &editorial.Note{Score: 0.2}}
	got := classifyTimedOutReview(recorded)
	if got != recorded {
		t.Fatalf("classifyTimedOutReview(%+v) = %+v, want it unchanged", recorded, got)
	}
}

func TestClassifyTimedOutReview_EmptyBecomesExit2(t *testing.T) {
	got := classifyTimedOutReview(editorial.ReviewResult{})
	if got.Exit != 2 {
		t.Fatalf("Exit = %d, want 2", got.Exit)
	}
	if got.Class != editorial.BackendTimeout {
		t.Fatalf("Class = %q, want %q", got.Class, editorial.BackendTimeout)
	}
}

// TestClassifyTimedOutReview_ApproveWithNoNoteIsNotTrusted guards the
// specific hazard this function exists for: an Exit==0 zero value is
// indistinguishable from "the goroutine was cancelled before assigning
// anything" and so must never be read as an approval.
func TestClassifyTimedOutReview_ApproveWithNoNoteIsNotTrusted(t *testing.T) {
	got := classifyTimedOutReview(editorial.ReviewResult{Exit: 0})
	if got.Exit != 2 {
		t.Fatalf("Exit = %d, want 2 (an Exit==0 with no Note must never stand as an approval)", got.Exit)
	}
}

func TestRunSuite_StopsAtFirstFailure(t *testing.T) {
	var ran []string
	run := func(ctx context.Context, step SuiteStep) StepResult {
		ran = append(ran, step.Name)
		if step.Name == "lint" {
			return StepResult{Success: false, Output: "lint findings"}
		}
		return StepResult{Success: true}
	}
	steps := []SuiteStep{
		{Name: "setup", Cmd: "true"},
		{Name: "lint", Cmd: "false"},
		{Name: "build", Cmd: "true"},
		{Name: "test", Cmd: "true"},
	}
	result := RunSuite(context.Background(), steps, run)
	if result.Success {
		t.Fatal("Success = true, want false")
	}
	if result.FailedStep != "lint" {
		t.Fatalf("FailedStep = %q, want %q", result.FailedStep, "lint")
	}
	if len(ran) != 2 {
		t.Fatalf("ran %v, want exactly [setup lint] — build/test must not run after lint fails", ran)
	}
}

func TestRunSuite_SkipsEmptyCommands(t *testing.T) {
	var ran []string
	run := func(ctx context.Context, step SuiteStep) StepResult {
		ran = append(ran, step.Name)
		return StepResult{Success: true}
	}
	steps := []SuiteStep{
		{Name: "setup", Cmd: ""},
		{Name: "typecheck", Cmd: "  "},
		{Name: "test", Cmd: "make test"},
	}
	result := RunSuite(context.Background(), steps, run)
	if !result.Success || !result.Ran {
		t.Fatalf("result = %+v, want Success and Ran true", result)
	}
	if len(ran) != 1 || ran[0] != "test" {
		t.Fatalf("ran = %v, want only [test]", ran)
	}
}

func TestRunSuite_NothingConfigured_PassesByDefault(t *testing.T) {
	run := func(ctx context.Context, step SuiteStep) StepResult {
		t.Fatal("run should never be called when every step is empty")
		return StepResult{}
	}
	result := RunSuite(context.Background(), []SuiteStep{{Name: "test", Cmd: ""}}, run)
	if !result.Success || result.Ran {
		t.Fatalf("result = %+v, want Success=true Ran=false", result)
	}
}
