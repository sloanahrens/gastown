package daemon

import (
	"context"
	"testing"
	"time"
)

// TestTierSweepRunnerStepContext: a parent with no deadline gets one no later
// than now plus the budget, a parent whose deadline is sooner keeps it, and a
// canceled parent stays canceled (gt-aoafom). The budget is an hour so no row
// waits on the clock; deadlines are read through ctx.Deadline().
func TestTierSweepRunnerStepContext(t *testing.T) {
	t.Parallel()
	const budget = time.Hour

	t.Run("no deadline gets one within the budget", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := tierSweepRunnerStepContext(context.Background(), budget)
		defer cancel()
		after := time.Now()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("step context has no deadline, want one bounded by the budget")
		}
		if latest := after.Add(budget); deadline.After(latest) {
			t.Fatalf("deadline %v is after now+budget %v", deadline, latest)
		}
	})

	t.Run("sooner parent deadline is kept", func(t *testing.T) {
		t.Parallel()
		parent, cancelParent := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
		defer cancelParent()
		want, _ := parent.Deadline()
		ctx, cancel := tierSweepRunnerStepContext(parent, budget)
		defer cancel()
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(want) {
			t.Fatalf("deadline = %v (ok=%v), want the parent's %v", got, ok, want)
		}
	})

	t.Run("canceled parent stays canceled", func(t *testing.T) {
		t.Parallel()
		parent, cancelParent := context.WithCancel(context.Background())
		cancelParent()
		ctx, cancel := tierSweepRunnerStepContext(parent, budget)
		defer cancel()
		if err := ctx.Err(); err != context.Canceled {
			t.Fatalf("step context Err() = %v, want context.Canceled", err)
		}
	})
}
