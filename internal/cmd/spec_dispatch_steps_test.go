package cmd

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specStepClock is a clock a test moves by hand, so a step's reported time is
// the amount the test's lint advanced it by, with no sleep (gt-dld6qn).
type specStepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *specStepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *specStepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestSpecDispatchCandidateStepsSumPerStep is the acceptance case: two
// candidates whose lint takes different amounts of the tick's clock report the
// sum under the lint step (gt-dld6qn).
func TestSpecDispatchCandidateStepsSumPerStep(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 2, "2026-09-29T11:00:00Z"),
	)
	clock := &specStepClock{now: specTestNow}
	env := f.env()
	env.Now = clock.Now
	env.PerTick = 2
	costs := map[string]time.Duration{"gt-a": 2 * time.Second, "gt-b": 5 * time.Second}
	env.Lint = func(s specdispatch.Spec) specdispatch.LintVerdict {
		clock.advance(costs[s.ID])
		return specdispatch.LintWith(s, env.Template, env.lintOptions())
	}
	r := runSpecDispatchCycle(env)
	if got, want := r.CandidatesSteps["lint"], 7*time.Second; got != want {
		t.Fatalf("CandidatesSteps[lint] = %v, want %v (report %+v)", got, want, r.CandidatesSteps)
	}
}

// TestSpecDispatchCandidateStepsHeldTick: a held tick runs no candidate, so it
// reports no step time and never lints (gt-dld6qn).
func TestSpecDispatchCandidateStepsHeldTick(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.hold = "operator hold"
	env := f.env()
	env.Lint = func(specdispatch.Spec) specdispatch.LintVerdict {
		t.Error("a held tick ran the candidates phase's lint")
		return specdispatch.LintVerdict{}
	}
	r := runSpecDispatchCycle(env)
	if r.CandidatesSteps != nil {
		t.Fatalf("CandidatesSteps = %+v, want none on a held tick", r.CandidatesSteps)
	}
}

// TestSpecDispatchCandidateStepsNilTimer: a caller handed no timer still runs
// the step it wraps and reports no time (gt-dld6qn).
func TestSpecDispatchCandidateStepsNilTimer(t *testing.T) {
	t.Parallel()
	var steps *specCandidateSteps
	ran := false
	steps.time("lint", func() { ran = true })
	steps.add("lint", time.Second)
	if !ran {
		t.Fatal("a nil timer skipped the step it wraps")
	}
	if got := steps.all(); got != nil {
		t.Fatalf("a nil timer reported %+v, want none", got)
	}
}

// TestSpecDispatchCandidateStepsEstimateSum: two candidates whose estimate
// takes different amounts of the tick's clock report the sum under the estimate
// step (gt-wga76n).
func TestSpecDispatchCandidateStepsEstimateSum(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 2, "2026-09-29T11:00:00Z"),
	)
	clock := &specStepClock{now: specTestNow}
	env := f.env()
	env.Now = clock.Now
	env.PerTick = 2
	env.StoryPointShadow = true
	costs := map[string]time.Duration{"gt-a": 3 * time.Second, "gt-b": 4 * time.Second}
	env.StoryPointEstimate = func(_ context.Context, id string) (specEstimateOutcome, error) {
		clock.advance(costs[id])
		return splitOutcome("estimated", 2, 5, "all", "nearest beads: gt-c"), nil
	}
	r := runSpecDispatchCycle(env)
	if got, want := r.CandidatesSteps["estimate"], 7*time.Second; got != want {
		t.Fatalf("CandidatesSteps[estimate] = %v, want %v (report %+v)", got, want, r.CandidatesSteps)
	}
}

// TestSpecDispatchCandidateStepsEstimateHeldBefore: a candidate the tick holds
// before the story-point step is never estimated, so its estimate time is not
// in the report (gt-wga76n).
func TestSpecDispatchCandidateStepsEstimateHeldBefore(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-held", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-slung", 2, "2026-09-29T11:00:00Z"),
	)
	held := f.specs["gt-held"]
	held.Metadata = contentionMetadata(specTestNow.Add(-time.Minute), 1, specTestNow.Add(time.Hour))
	f.specs["gt-held"] = held
	clock := &specStepClock{now: specTestNow}
	env := f.env()
	env.Now = clock.Now
	env.PerTick = 2
	env.StoryPointSplitOver = 15
	var estimated []string
	env.StoryPointEstimate = func(_ context.Context, id string) (specEstimateOutcome, error) {
		estimated = append(estimated, id)
		clock.advance(4 * time.Second)
		return splitOutcome("estimated", 2, 5, "all", "nearest beads: gt-c"), nil
	}
	r := runSpecDispatchCycle(env)
	if want := []string{"gt-slung"}; !slices.Equal(estimated, want) {
		t.Fatalf("estimated %v, want only %v", estimated, want)
	}
	if got, want := r.CandidatesSteps["estimate"], 4*time.Second; got != want {
		t.Fatalf("CandidatesSteps[estimate] = %v, want %v (report %+v)", got, want, r.CandidatesSteps)
	}
}

// TestSpecDispatchCandidateStepsEstimateHoldsCounted: the step that holds a
// bead for splitting is still timed under estimate (gt-wga76n).
func TestSpecDispatchCandidateStepsEstimateHoldsCounted(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	clock := &specStepClock{now: specTestNow}
	env := f.env()
	env.Now = clock.Now
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		clock.advance(9 * time.Second)
		return splitOutcome("estimated", 13, 20, "all", "nearest beads: gt-b"), nil
	}
	r := runSpecDispatchCycle(env)
	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want the bead held for splitting", f.slung)
	}
	if len(splitHoldLines(r.Skipped)) == 0 {
		t.Fatalf("skipped = %+v, want the split hold line", r.Skipped)
	}
	if got, want := r.CandidatesSteps["estimate"], 9*time.Second; got != want {
		t.Fatalf("CandidatesSteps[estimate] = %v, want %v (report %+v)", got, want, r.CandidatesSteps)
	}
}

// TestSpecDispatchCandidateStepsLineNamesTheStepsInNameOrder: the cost line
// names each candidates-phase step with its summed time in name order, so the
// line reads the same from run to run (gt-3xfcel).
func TestSpecDispatchCandidateStepsLineNamesTheStepsInNameOrder(t *testing.T) {
	t.Parallel()
	steps := map[string]time.Duration{
		"verdict": 3 * time.Second, "lint": 900 * time.Millisecond,
		"estimate": 7 * time.Second, "shape": 1500 * time.Millisecond,
	}
	got := specTickCost(newSpecTickReads(), time.Second, 0, 0, 0, 0, 0, steps)
	want := "tick cost: 0 bd read call(s) outside the sling path in 1s; candidate steps estimate 7s, lint 900ms, shape 1.5s, verdict 3s"
	if got != want {
		t.Fatalf("cost line = %q, want %q", got, want)
	}
}

// TestSpecDispatchCandidateStepsLineUnchangedWithNone: a tick whose candidates
// phase timed no step prints the line it always has (gt-3xfcel).
func TestSpecDispatchCandidateStepsLineUnchangedWithNone(t *testing.T) {
	t.Parallel()
	want := "tick cost: 0 bd read call(s) outside the sling path in 1.5s; 3 sling(s) in parallel, slowest 900ms"
	for _, steps := range []map[string]time.Duration{nil, {}} {
		if got := specTickCost(newSpecTickReads(), 1500*time.Millisecond, 3, 900*time.Millisecond, 0, 0, 0, steps); got != want {
			t.Fatalf("cost line with %v steps = %q, want %q", steps, got, want)
		}
	}
}
