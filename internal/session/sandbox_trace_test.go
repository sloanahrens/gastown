package session

import (
	"errors"
	"testing"
	"time"
)

// traceClock is a clock the test moves by hand, so every duration a trace
// records is one the test set.
type traceClock struct{ now time.Time }

func newTraceClock() *traceClock { return &traceClock{now: time.Unix(1_000_000, 0)} }

func (c *traceClock) Now() time.Time          { return c.now }
func (c *traceClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestSandboxTracePhaseReturnsItsErrorAndRecordsIt(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	refused := errors.New("image drift")

	err := trace.Phase("gt check", func() error {
		clock.Advance(3 * time.Second)
		return refused
	})

	if !errors.Is(err, refused) {
		t.Fatalf("Phase returned %v, want the phase's own error %v", err, refused)
	}
	if got, want := trace.Summary(), "gt check 3s, total 3s"; got != want {
		t.Fatalf("Summary() = %q, want %q: a failed phase is still a phase the start ran", got, want)
	}
}

func TestSandboxTraceMarkCoversTheGapSincePhaseEnded(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	_ = trace.Phase("ready", func() error {
		clock.Advance(4 * time.Second)
		return nil
	})
	clock.Advance(2 * time.Second)

	trace.Mark("start")

	if got, want := trace.Summary(), "ready 4s, start 2s, total 6s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestSandboxTraceSummaryIsEmptyUntilAPhaseRuns(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	clock.Advance(time.Minute)

	if got := trace.Summary(); got != "" {
		t.Fatalf("Summary() = %q, want empty: a sling that never reached a sandbox logs nothing", got)
	}
}

// The time between the trace's start and its first mark, and after its last
// one, is a phase of its own: without it the note's phases would fall short of
// its total by exactly the time nothing named (gt-7nctk).
func TestSandboxTraceSummaryCarriesTheUnnamedTimeAsOther(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	clock.Advance(2 * time.Second)
	trace.Mark("guards")
	clock.Advance(3 * time.Second)
	trace.Mark("hook")
	clock.Advance(5 * time.Second)

	if got, want := trace.Summary(), "guards 2s, hook 3s, other 5s, total 10s"; got != want {
		t.Fatalf("Summary() = %q, want %q: the time no phase named is reported rather than lost", got, want)
	}
}

// Steps hands the trace's Mark down a call chain, so a stage a caller reports
// lands on the same timeline as the phases the start times itself.
func TestSandboxTraceStepsRecordsOnTheTrace(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	steps := trace.Steps()
	clock.Advance(2 * time.Second)
	steps("seat decision")
	clock.Advance(3 * time.Second)
	steps("hook")

	if got, want := trace.Summary(), "seat decision 2s, hook 3s, total 5s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
	var nilTrace *SandboxTrace
	if nilTrace.Steps() != nil {
		t.Error("(*SandboxTrace)(nil).Steps() returned a sink, want nil so a caller can pass it on unguarded")
	}
}

func TestSandboxTraceNilRunsThePhaseButTimesNothing(t *testing.T) {
	t.Parallel()
	var trace *SandboxTrace
	ran := false
	refused := errors.New("refused")

	err := trace.Phase("create", func() error {
		ran = true
		return refused
	})
	trace.Mark("start")

	if !ran || !errors.Is(err, refused) {
		t.Fatalf("nil trace Phase ran=%v err=%v, want the phase to run and return its error", ran, err)
	}
	if got := trace.Summary(); got != "" {
		t.Fatalf("nil trace Summary() = %q, want empty", got)
	}
}
