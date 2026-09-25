package lintlock

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubDelay shortens the waits so a test can drive the retry loop without
// sleeping the real tens of seconds.
func stubDelay(t *testing.T, delays ...time.Duration) {
	t.Helper()
	prev := RetryDelay
	RetryDelay = delays
	t.Cleanup(func() { RetryDelay = prev })
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
	stubDelay(t, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), MinBudget())
	defer cancel()

	attempts := 0
	outcome := Retry(ctx, func() Attempt {
		attempts++
		if attempts == 1 {
			return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
		}
		return Attempt{Err: errors.New("exit 1"), Output: "pkga/a.go:1:1: something is wrong (fakecheck)"}
	}, nil)

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
//
// Serial because stubDelay swaps the package-level RetryDelay and restores it
// (gt-k317). Install-once is not available here: each test shortens the delay
// to a different value.
func TestRetry_ContentionIsWaitedOut(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"the lock marker", "Error: " + Marker},
		{"golangci-lint's own timeout", "level=error msg=\"" + TimeoutSentinel + "\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDelay(t, time.Millisecond)

			ctx, cancel := context.WithTimeout(context.Background(), MinBudget())
			defer cancel()

			attempts := 0
			outcome := Retry(ctx, func() Attempt {
				attempts++
				if attempts == 1 {
					return Attempt{Err: errors.New("exit 2"), Output: tc.output}
				}
				return Attempt{}
			}, nil)

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
	stubDelay(t, time.Millisecond)

	alwaysContended := func(attempts *int) func() Attempt {
		return func() Attempt {
			*attempts++
			return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
		}
	}

	t.Run("a budget with no room left for the lint itself: no retry", func(t *testing.T) {
		// The deadline is exactly the reserve, so even a 1ms wait would leave
		// less than a lint's worth of budget.
		ctx, cancel := context.WithTimeout(context.Background(), Reserve)
		defer cancel()

		attempts := 0
		outcome := Retry(ctx, alwaysContended(&attempts), nil)
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
		attempts := 0
		outcome := Retry(context.Background(), alwaysContended(&attempts), nil)
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 (an unbounded context must not drive an unbounded retry loop)", attempts)
		}
		if outcome.Err == nil {
			t.Error("Err = nil, want the attempt's error")
		}
	})
}

func TestRetry_StopsWhenTheScheduleRunsOut(t *testing.T) {
	stubDelay(t, time.Millisecond, time.Millisecond)

	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), MinBudget())
	defer cancel()
	outcome := Retry(ctx, func() Attempt {
		attempts++
		return Attempt{Err: errors.New("exit 2"), Output: "Error: " + Marker}
	}, nil)

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
	ctx, cancel := context.WithTimeout(context.Background(), MinBudget())
	defer cancel()

	if !RoomForRetry(ctx, time.Second) {
		t.Error("RoomForRetry = false on a fresh MinBudget context, want true")
	}

	tight, tightCancel := context.WithTimeout(context.Background(), Reserve+time.Millisecond)
	defer tightCancel()
	if RoomForRetry(tight, time.Second) {
		t.Error("RoomForRetry = true when the wait would eat the lint's reserve, want false")
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
