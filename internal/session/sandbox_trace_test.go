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
