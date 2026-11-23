package metrics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stepBudget is the run's budget context in a test: its Done closes when the step
// the row means calls end, and Err then reads context.DeadlineExceeded.
type stepBudget struct {
	context.Context // the caller's context, for Value
	done            chan struct{}
	once            sync.Once
}

// context is the seam RunStoryPointsWeekly makes its budget context with.
func (b *stepBudget) context(context.Context, time.Duration) (context.Context, context.CancelFunc) {
	return b, func() {}
}

func (b *stepBudget) end()                  { b.once.Do(func() { close(b.done) }) }
func (b *stepBudget) Done() <-chan struct{} { return b.done }
func (b *stepBudget) Err() error {
	select {
	case <-b.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// budgetStepEmbedder ends the run's budget when first called, then waits for it;
// it refuses a context the seam did not make, so a skipped seam fails, not hangs.
type budgetStepEmbedder struct{ step *stepBudget }

func (e budgetStepEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if ctx != context.Context(e.step) {
		return nil, fmt.Errorf("embedder called with a context the budget seam did not make")
	}
	e.step.end()
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStoryPointsWeeklyBudget: a due run ends itself at its budget. Its end
// while wiring the town, at the week-after check or at the retrain is a wait:
// nil error, the waiting line naming that step, no stamp, and a failed progress
// write still returned. A step error carrying context.DeadlineExceeded without the
// run's budget having ended spends the week, as an ordinary failure does. A row
// that ends at the budget fills Commands.BudgetContext with a stepBudget, so the
// run ends at the step the row means and not on a timer.
func TestStoryPointsWeeklyBudget(t *testing.T) {
	t.Parallel()
	// step is the step a budget wait must name; an empty step fails for cause.
	for _, tc := range []struct {
		name                                             string
		adoption, failWrite, wireWait, inner, callerEnds bool
		step, cause                                      string
	}{
		{"retrain waits", false, false, false, false, false, "retraining", ""},
		{"adoption check waits", true, false, false, false, false, "checking the last adoption", ""},
		{"the wire waits", false, false, true, false, false, "wiring the town", ""},
		{"progress write failure is returned", false, true, false, false, false, "retraining", ""},
		{"an inner deadline is not the budget", false, false, false, true, false, "", "context deadline exceeded"},
		{"the caller's own deadline is not the budget", false, false, true, false, true, "", "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			last := weeklyNow.AddDate(0, 0, -8)
			writeWeeklyLastRun(t, fix.town, last)
			if tc.adoption {
				writeAt(t, adoptRuntimePath(fix.town), adoptTestWeights{boxes: 0.1, intercept: 1}.JSON())
				writeAt(t, storyPointsAdoptionsPath(fix.town), fmt.Sprintf("{\"event\":\"adopted\",\"at\":%q,\"version\":\"v1\",\"previous\":\"\"}\n",
					weeklyNow.Add(-storyPointsCheckAge-24*time.Hour).Format(time.RFC3339)))
			}
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), budgetStepEmbedder{})
			var step *stepBudget
			if tc.step != "" {
				step = &stepBudget{Context: context.Background(), done: make(chan struct{})}
				c.BudgetContext = step.context
				c.Embedder = budgetStepEmbedder{step: step}
			}
			if tc.wireWait {
				// The seam's wire ends the budget itself; without one (the caller's
				// deadline row) the caller's context ends it.
				c.Wire = func(ctx context.Context) (RealTown, error) {
					if step != nil {
						step.end()
					}
					<-ctx.Done()
					return RealTown{}, ctx.Err()
				}
			}
			if tc.failWrite {
				c.Progress = failingWriter{}
			}
			if tc.inner {
				c.Embedder = trainTestEmbedder{}
				c.Env.ReadFile = func(path string) ([]byte, error) {
					if strings.HasSuffix(path, landingsName) {
						return nil, fmt.Errorf("read %s: %w", path, context.DeadlineExceeded)
					}
					return os.ReadFile(path)
				}
			}

			ctx := context.Background()
			if tc.callerEnds {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Unix(1, 0))
				defer stop()
			}
			err := c.RunStoryPointsWeekly(ctx)
			line := out.String()
			wantStamp := last.Format(time.RFC3339)
			switch {
			case tc.callerEnds:
				if err == nil || !strings.Contains(err.Error(), tc.cause) || strings.Contains(line, "waiting") {
					t.Fatalf("err = %v, out = %q, want the caller's deadline returned and no waiting line", err, line)
				}
			case tc.cause != "":
				wantStamp = weeklyNow.UTC().Format(time.RFC3339)
				if err == nil || !strings.HasPrefix(line, "story points weekly: failed: ") || !strings.Contains(line, tc.cause) || !strings.Contains(line, "the week is spent") {
					t.Fatalf("err = %v, out = %q, want the failure, its cause and that the week is spent", err, line)
				}
			case tc.failWrite:
				if err == nil || !strings.Contains(err.Error(), "closed pipe") || !strings.HasPrefix(line, "story points weekly: waiting: ") || !strings.Contains(line, "while retraining") {
					t.Fatalf("err = %v, out = %q, want the wait's line and the progress failure returned", err, line)
				}
			default:
				if err != nil {
					t.Fatalf("RunStoryPointsWeekly err = %v, want nil: the budget's end is a wait", err)
				}
				if want := fmt.Sprintf("story points weekly: waiting: the %s budget ended while %s; the next hour continues from the embedding cache", StoryPointsWeeklyBudget, tc.step); strings.TrimSpace(line) != want {
					t.Errorf("out = %q, want %q: the wait naming the step and the cache", line, want)
				}
				if !tc.adoption {
					requireNoWeeklyWeights(t, fix.town)
				}
			}
			if got := weeklyLastRun(t, fix.town); got != wantStamp {
				t.Errorf("stamp = %q, want %q", got, wantStamp)
			}
		})
	}
}
