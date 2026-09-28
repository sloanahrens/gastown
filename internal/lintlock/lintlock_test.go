package lintlock

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// testEpoch is the fixed start of every fake clock in this package's tests.
var testEpoch = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// deadlineCtx is a context that never ends but reports a deadline, so the
// budget checks can be measured against a fake clock while the only thing
// waiting on that clock is Retry's own timer.
type deadlineCtx struct {
	context.Context
	deadline time.Time
}

func (c deadlineCtx) Deadline() (time.Time, bool) { return c.deadline, true }

// budget returns a fake clock at testEpoch and a context whose deadline is d
// past it.
func budget(d time.Duration) (*clockwork.FakeClock, context.Context) {
	return clockwork.NewFakeClockAt(testEpoch), deadlineCtx{context.Background(), testEpoch.Add(d)}
}

// runRetry runs retry with delays on clk, advancing clk through each wait,
// and returns its outcome. It fails the test if retry neither returns nor
// waits within 10 s.
func runRetry(t *testing.T, ctx context.Context, clk *clockwork.FakeClock, delays []time.Duration, attempt func() Attempt) Outcome {
	t.Helper()
	done := make(chan Outcome, 1)
	go func() { done <- retry(ctx, clk, delays, attempt, nil) }()
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(wait, 1) }()
		select {
		case o := <-done:
			return o
		case err := <-blocked:
			if err != nil {
				t.Fatalf("retry neither returned nor waited on the clock: %v", err)
			}
			clk.Advance(time.Minute)
		}
	}
}

func TestContendedAndUnfinished(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		output         string
		wantContended  bool
		wantUnfinished bool
	}{
		{"the lock marker", "Error: " + Marker + "\n", true, true},
		{"golangci-lint's own timeout", "level=error msg=\"" + TimeoutSentinel + "\"\n", false, true},
		{"a finding", "pkga/a.go:1:1: something is wrong (fakelint)\n", false, false},
		{"a different lint failure", "can't load config: unsupported version\n", false, false},
		{"empty output", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Contended(tc.output); got != tc.wantContended {
				t.Errorf("Contended(%q) = %v, want %v", tc.output, got, tc.wantContended)
			}
			if got := Unfinished(tc.output); got != tc.wantUnfinished {
				t.Errorf("Unfinished(%q) = %v, want %v", tc.output, got, tc.wantUnfinished)
			}
		})
	}
}

// TestRetry_RealFailureAfterContentionIsReported is the regression the refinery
// gate asked for (gt-ijqw, om-gate attempt 1 major): the final attempt decides
// the verdict, not the retry count. A lint that contends once and then reports
// real errors must be forwarded as findings — reporting it as "nothing was
// linted" sends the agent to re-run a lint that will fail identically.
func TestRetry_RealFailureAfterContentionIsReported(t *testing.T) {
	t.Parallel()
	clk, ctx := budget(MinBudget())

	attempts := 0
	outcome := runRetry(t, ctx, clk, RetryDelay, func() Attempt {
		attempts++
		if attempts == 1 {
			return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
		}
		return Attempt{Err: errors.New("exit 1"), Output: "pkga/a.go:1:1: something is wrong (fakecheck)"}
	})

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one wait, then the retry)", attempts)
	}
	if outcome.Waits != 1 {
		t.Errorf("Waits = %d, want 1", outcome.Waits)
	}
	if outcome.Contended || outcome.Unfinished {
		t.Errorf("final attempt verdicts = (contended=%v, unfinished=%v), want both false: the final attempt reported a finding", outcome.Contended, outcome.Unfinished)
	}
	if outcome.Output != "pkga/a.go:1:1: something is wrong (fakecheck)" {
		t.Errorf("Output = %q, want the final attempt's own output", outcome.Output)
	}
}

// TestRetry_ContentionIsWaitedOut pins the behaviour the retry exists for, in
// both shapes a lint that never reported findings takes: the marker a contended
// golangci-lint prints, and the timeout of one that took the lock and outran
// run.timeout (gt-taoz, gt-ijqw).
func TestRetry_ContentionIsWaitedOut(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		output string
	}{
		{"the lock marker", "Error: " + Marker},
		{"golangci-lint's own timeout", "level=error msg=\"" + TimeoutSentinel + "\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, ctx := budget(MinBudget())

			attempts := 0
			outcome := runRetry(t, ctx, clk, RetryDelay, func() Attempt {
				attempts++
				if attempts == 1 {
					return Attempt{Err: errors.New("exit 2"), Output: tc.output}
				}
				return Attempt{}
			})

			if attempts != 2 {
				t.Errorf("attempts = %d, want 2 (a lint that never ran is retried)", attempts)
			}
			if outcome.Err != nil {
				t.Errorf("Err = %v, want nil after the retry succeeded", outcome.Err)
			}
			if outcome.Waits != 1 {
				t.Errorf("Waits = %d, want 1", outcome.Waits)
			}
		})
	}
}

// TestRetry_BudgetBoundsRetry pins the two ways the loop refuses to outlive its
// budget: it never starts a retry the budget cannot also fit a lint into, and
// it refuses a context with no deadline outright.
func TestRetry_BudgetBoundsRetry(t *testing.T) {
	t.Parallel()
	delays := []time.Duration{time.Millisecond}

	alwaysContended := func(attempts *int) func() Attempt {
		return func() Attempt {
			*attempts++
			return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
		}
	}

	t.Run("a budget with no room left for the lint itself: no retry", func(t *testing.T) {
		t.Parallel()
		// The deadline is exactly the reserve, so even a 1ms wait would leave
		// less than a lint's worth of budget.
		clk, ctx := budget(Reserve)

		attempts := 0
		outcome := runRetry(t, ctx, clk, delays, alwaysContended(&attempts))
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 (no retry may eat the lint's own budget)", attempts)
		}
		if outcome.Waits != 0 {
			t.Errorf("Waits = %d, want 0", outcome.Waits)
		}
		if outcome.Err == nil {
			t.Error("Err = nil, want the attempt's error")
		}
	})

	t.Run("a context with no deadline: no retry", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		outcome := runRetry(t, context.Background(), clockwork.NewFakeClockAt(testEpoch), delays, alwaysContended(&attempts))
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 (an unbounded context must not drive an unbounded retry loop)", attempts)
		}
		if outcome.Err == nil {
			t.Error("Err = nil, want the attempt's error")
		}
	})
}

func TestRetry_StopsWhenTheScheduleRunsOut(t *testing.T) {
	t.Parallel()
	clk, ctx := budget(MinBudget())

	attempts := 0
	outcome := runRetry(t, ctx, clk, []time.Duration{time.Millisecond, time.Millisecond}, func() Attempt {
		attempts++
		return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
	})

	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (one plus one per scheduled wait)", attempts)
	}
	if outcome.Waits != 2 {
		t.Errorf("Waits = %d, want 2", outcome.Waits)
	}
	if !outcome.Contended {
		t.Error("Contended = false, want true: the last attempt still showed the marker")
	}
}

func TestRoomForRetry(t *testing.T) {
	t.Parallel()
	clk, ctx := budget(MinBudget())
	if !roomForRetry(ctx, clk, time.Second) {
		t.Error("RoomForRetry = false on a fresh MinBudget context, want true")
	}

	clk, tight := budget(Reserve + time.Millisecond)
	if roomForRetry(tight, clk, time.Second) {
		t.Error("RoomForRetry = true when the wait would eat the lint's reserve, want false")
	}

	clk, exact := budget(Reserve + time.Second)
	if !roomForRetry(exact, clk, time.Second) {
		t.Error("RoomForRetry = false when the wait plus the reserve exactly fit, want true")
	}
	clk.Advance(time.Nanosecond)
	if roomForRetry(exact, clk, time.Second) {
		t.Error("RoomForRetry = true once the clock moved past the fit, want false")
	}

	if roomForRetry(context.Background(), clk, 0) {
		t.Error("RoomForRetry = true for a context with no deadline, want false")
	}
}

// TestRetry_WaitsTheScheduleOnTheClock pins that each retry waits exactly its
// scheduled delay: an attempt never starts before the clock has moved by it.
func TestRetry_WaitsTheScheduleOnTheClock(t *testing.T) {
	t.Parallel()
	clk, ctx := budget(MinBudget())
	delays := []time.Duration{15 * time.Second, 30 * time.Second}
	var at []time.Time
	var waits []int
	done := make(chan Outcome, 1)
	go func() {
		done <- retry(ctx, clk, delays, func() Attempt {
			at = append(at, clk.Now())
			return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
		}, func(attempt, attempts int, wait time.Duration) {
			if attempts != 3 {
				t.Errorf("onRetry attempts = %d, want 3", attempts)
			}
			waits = append(waits, attempt)
		})
	}()
	for _, d := range delays {
		if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(d)
	}
	o := <-done
	want := []time.Time{testEpoch, testEpoch.Add(15 * time.Second), testEpoch.Add(45 * time.Second)}
	if len(at) != len(want) {
		t.Fatalf("attempts at %v, want %v", at, want)
	}
	for i := range want {
		if !at[i].Equal(want[i]) {
			t.Errorf("attempt %d at %v, want %v", i+1, at[i], want[i])
		}
	}
	if len(waits) != 2 || waits[0] != 1 || waits[1] != 2 {
		t.Errorf("onRetry attempts = %v, want [1 2]", waits)
	}
	if o.Waits != 2 {
		t.Errorf("Waits = %d, want 2", o.Waits)
	}
}

// TestRetry_ContextEndsDuringWait pins that a context ending mid-wait returns
// the last attempt's outcome rather than starting another attempt.
func TestRetry_ContextEndsDuringWait(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(testEpoch)
	parent, cancel := context.WithCancel(context.Background())
	ctx := deadlineCtx{parent, testEpoch.Add(MinBudget())}
	attempts := 0
	done := make(chan Outcome, 1)
	go func() {
		done <- retry(ctx, clk, RetryDelay, func() Attempt {
			attempts++
			return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
		}, nil)
	}()
	if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	o := <-done
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: the context ended during the first wait", attempts)
	}
	if o.Err == nil || !o.Contended {
		t.Errorf("outcome = %+v, want the first attempt's contended failure", o)
	}
}

// TestRetry_ExportedUsesRetryDelayAndRealClock pins the exported wrapper on a
// path that never waits: a first attempt that succeeds.
func TestRetry_ExportedUsesRetryDelayAndRealClock(t *testing.T) {
	t.Parallel()
	o := Retry(context.Background(), func() Attempt { return Attempt{} }, nil)
	if o.Err != nil || o.Waits != 0 {
		t.Errorf("Retry = %+v, want a clean first attempt", o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), MinBudget())
	defer cancel()
	if !RoomForRetry(ctx, time.Second) {
		t.Error("RoomForRetry = false on a fresh MinBudget context, want true")
	}
}

// TestBudgetsFitTheGateTheScheduleWasBuiltFor keeps the policy's numbers
// honest: the waits are sized for a 10m lint gate, which is the budget both
// callers give theirs, and MinBudget must not exceed it (gt-xsty).
func TestBudgetsFitTheGateTheScheduleWasBuiltFor(t *testing.T) {
	t.Parallel()
	if got, want := Schedule(), 300*time.Second; got != want {
		t.Errorf("Schedule() = %v, want %v (15+30+30+45*5)", got, want)
	}
	if MinBudget() > 10*time.Minute {
		t.Errorf("MinBudget() = %v, want it to fit inside a 10m lint gate", MinBudget())
	}
}

// TestOutcomeLockWait pins the phrasing callers drop into their own failure
// detail: a gate with no budget to wait in must still read as one whose lock
// was held, not as one that waited zero times.
func TestOutcomeLockWait(t *testing.T) {
	t.Parallel()
	waited := Outcome{Err: errors.New("exit 2"), Waits: 2, Contended: true, Unfinished: true}
	if got := waited.LockWait(); !strings.Contains(got, "across 2 retries") {
		t.Errorf("LockWait() = %q, want it to name the 2 retries", got)
	}
	never := Outcome{Err: errors.New("exit 2"), Contended: true, Unfinished: true}
	if got := never.LockWait(); strings.Contains(got, "0 retries") {
		t.Errorf("LockWait() = %q, want it to avoid naming zero retries", got)
	}
}
