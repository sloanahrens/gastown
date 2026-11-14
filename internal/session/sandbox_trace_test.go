package session

import (
	"errors"
	"strings"
	"sync"
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

// A note rides on the phase recorded last, after its duration, so a wait's
// reason sits beside the phase it explains.
func TestSandboxTraceNoteAnnotatesThePhaseRecordedLast(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Phase("create", func() error { clock.Advance(time.Second); return nil })
	trace.Phase("ready", func() error { clock.Advance(4 * time.Second); return nil })
	trace.Note("DependenciesNotReady: Pod is pending")

	if got, want := trace.Summary(), "create 1s, ready 4s (DependenciesNotReady: Pod is pending), total 5s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// An empty note, a trace with no phase, and a nil trace all leave the summary
// as it was: a wait with no reason adds nothing to the log.
func TestSandboxTraceNoteIsANoOpWithoutAPhaseOrText(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Note("nothing yet")
	if got := trace.Summary(); got != "" {
		t.Fatalf("Summary() before any phase = %q, want empty", got)
	}

	trace.Phase("ready", func() error { clock.Advance(time.Second); return nil })
	trace.Note("")
	if got, want := trace.Summary(), "ready 1s, total 1s"; got != want {
		t.Fatalf("Summary() after an empty note = %q, want %q", got, want)
	}

	trace.Note(" \n\t ")
	if got, want := trace.Summary(), "ready 1s, total 1s"; got != want {
		t.Fatalf("Summary() after a whitespace-only note = %q, want %q", got, want)
	}

	trace.Note("\x00\x1b[31m\x7f")
	if got, want := trace.Summary(), "ready 1s, total 1s"; got != want {
		t.Fatalf("Summary() after a control-bytes-only note = %q, want %q", got, want)
	}

	var nilTrace *SandboxTrace
	nilTrace.Note("ignored")
}

// A gateway's condition message can carry newlines and tabs; the summary is one
// log line, so the note reaches it collapsed rather than verbatim.
func TestSandboxTraceNoteStaysOnOneLineInTheSummary(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Phase("ready", func() error { clock.Advance(time.Second); return nil })
	trace.Note("DependenciesNotReady: Pod is pending\n0/3 nodes are available,\tInsufficient memory")

	want := "ready 1s (DependenciesNotReady: Pod is pending 0/3 nodes are available, Insufficient memory), total 1s"
	if got := trace.Summary(); got != want {
		t.Fatalf("Summary() = %q, want %q: a multi-line message must not break the log line", got, want)
	}
}

// Gateway text can carry terminal escape sequences and control bytes; the
// summary is one log line for a terminal, so none of them survive into it.
func TestSandboxTraceNoteDropsControlBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, note, want string
	}{
		{"an ANSI colour sequence", "\x1b[31mDependenciesNotReady\x1b[0m: Pod is pending", "DependenciesNotReady: Pod is pending"},
		{"a carriage return", "Pod is pending\r0/3 nodes are available", "Pod is pending 0/3 nodes are available"},
		{"a NUL", "Pod\x00is pending", "Pod is pending"},
		{"a tab inside a note", "Pod\tis pending", "Pod is pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newTraceClock()
			trace := NewSandboxTrace(clock.Now)
			trace.Phase("ready", func() error { clock.Advance(time.Second); return nil })
			trace.Note(tc.note)

			want := "ready 1s (" + tc.want + "), total 1s"
			if got := trace.Summary(); got != want {
				t.Fatalf("Summary() = %q, want %q", got, want)
			}
		})
	}
}

// A gateway message has no length bound the town can rely on; a note longer
// than the summary's budget is cut rather than written whole into the log.
func TestSandboxTraceNoteIsCutToTheLogLineBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, note, want string
	}{
		{"exactly the budget", strings.Repeat("x", traceNoteMax), strings.Repeat("x", traceNoteMax)},
		{"over the budget", strings.Repeat("x", traceNoteMax+50), strings.Repeat("x", traceNoteMax) + " …"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newTraceClock()
			trace := NewSandboxTrace(clock.Now)
			trace.Phase("ready", func() error { clock.Advance(time.Second); return nil })
			trace.Note(tc.note)

			want := "ready 1s (" + tc.want + "), total 1s"
			if got := trace.Summary(); got != want {
				t.Fatalf("Summary() = %q, want %q", got, want)
			}
		})
	}
}

// A phase that made bd or git calls shows both counts beside its duration, so
// the log says how many calls a slow phase made as well as how long it took.
func TestSandboxTraceSummaryListsCallsBesideEachPhaseDuration(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Phase("guards", func() error {
		trace.CountBD()
		clock.Advance(time.Second)
		return nil
	})
	trace.Phase("ready", func() error {
		trace.CountBD()
		trace.CountBD()
		trace.CountGit()
		clock.Advance(4 * time.Second)
		return nil
	})

	if got, want := trace.Summary(), "guards 1s [bd 1, git 0], ready 4s [bd 2, git 1], total 5s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// Two slings run together keep their counts apart: each trace counts only the
// calls made on it.
func TestSandboxTraceCountsOfTwoTracesStayApart(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	first := NewSandboxTrace(clock.Now)
	second := NewSandboxTrace(clock.Now)
	first.CountBD()
	second.CountGit()
	second.CountGit()
	first.CountBD()
	first.CountGit()
	clock.Advance(time.Second)

	first.Mark("start")
	second.Mark("start")

	if got, want := first.Summary(), "start 1s [bd 2, git 1], total 1s"; got != want {
		t.Fatalf("first trace Summary() = %q, want %q", got, want)
	}
	if got, want := second.Summary(), "start 1s [bd 0, git 2], total 1s"; got != want {
		t.Fatalf("second trace Summary() = %q, want %q", got, want)
	}
}

// A phase with no calls prints no calls field, so a log from before counting
// is unchanged for the phases that made none.
func TestSandboxTraceSummaryOmitsCallsForAPhaseWithNone(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Phase("create", func() error { clock.Advance(time.Second); return nil })
	trace.Phase("ready", func() error {
		trace.CountGit()
		clock.Advance(4 * time.Second)
		return nil
	})

	if got, want := trace.Summary(), "create 1s, ready 4s [bd 0, git 1], total 5s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// The calls sit between the duration and the note, so the note still follows
// the phase it explains.
func TestSandboxTraceCallsSitBesideDurationBeforeTheNote(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Phase("ready", func() error {
		trace.CountBD()
		clock.Advance(4 * time.Second)
		return nil
	})
	trace.Note("DependenciesNotReady: Pod is pending")

	if got, want := trace.Summary(), "ready 4s [bd 1, git 0] (DependenciesNotReady: Pod is pending), total 4s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// Calls made in the gap no phase timed are charged to the next record, as the
// gap's time is, so the count sits beside the time it was spent in.
func TestSandboxTraceChargesUntimedCallsToTheNextPhase(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	trace.Phase("ready", func() error {
		trace.CountBD()
		clock.Advance(4 * time.Second)
		return nil
	})
	clock.Advance(2 * time.Second)
	trace.CountGit()
	trace.CountGit()

	trace.Mark("start")

	if got, want := trace.Summary(), "ready 4s [bd 1, git 0], start 2s [bd 0, git 2], total 6s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// Counts from many goroutines at once all land: the counters are atomic, so a
// call made off the sling's goroutine is neither lost nor double-counted.
func TestSandboxTraceCountsAreSafeForConcurrentCalls(t *testing.T) {
	t.Parallel()
	clock := newTraceClock()
	trace := NewSandboxTrace(clock.Now)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				trace.CountBD()
				trace.CountGit()
			}
		}()
	}
	wg.Wait()

	trace.Mark("start")

	if got, want := trace.Summary(), "start 0s [bd 10000, git 10000], total 0s"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// A nil trace counts nothing and does not panic, as its other methods do, so a
// caller can count without guarding the trace.
func TestSandboxTraceNilCountsAreNoOps(t *testing.T) {
	t.Parallel()
	var trace *SandboxTrace
	trace.CountBD()
	trace.CountGit()

	if got := trace.Summary(); got != "" {
		t.Fatalf("nil trace Summary() = %q, want empty", got)
	}
}
