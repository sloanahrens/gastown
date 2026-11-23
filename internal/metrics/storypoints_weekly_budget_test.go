package metrics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// budgetBlockingEmbedder waits for its context to end; without a deadline it refuses.
type budgetBlockingEmbedder struct{}

func (budgetBlockingEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("embedder called without a deadline")
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStoryPointsWeeklyBudget: a due run ends itself at Commands.Budget. Its end
// while wiring the town, at the week-after check or at the retrain is a wait:
// nil error, the waiting line naming that step, no stamp, and a failed progress
// write still returned. A step error carrying context.DeadlineExceeded without the
// run's budget having ended spends the week, as an ordinary failure does.
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
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), budgetBlockingEmbedder{})
			// Only a row that ends at the budget blocks on it, so only those take
			// one; a case that does real work keeps the default ten minutes.
			if tc.step != "" {
				c.Budget = 5 * time.Millisecond
			}
			if tc.wireWait {
				c.Wire = func(ctx context.Context) (RealTown, error) { <-ctx.Done(); return RealTown{}, ctx.Err() }
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
				if want := fmt.Sprintf("story points weekly: waiting: the 5ms budget ended while %s; the next hour continues from the embedding cache", tc.step); strings.TrimSpace(line) != want {
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
