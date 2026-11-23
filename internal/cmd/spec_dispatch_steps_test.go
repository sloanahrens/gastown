package cmd

import (
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
