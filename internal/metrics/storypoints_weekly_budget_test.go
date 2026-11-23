package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// budgetBlockingEmbedder waits for its context to end and answers with its
// error; a call without a deadline is refused at once instead of hanging.
type budgetBlockingEmbedder struct{}

func (budgetBlockingEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("embedder called without a deadline")
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStoryPointsWeeklyBudget: a due run ends itself at Commands.Budget. Its end
// while wiring the town, at the week-after check or at the retrain is a wait:
// nil error, the waiting line, no stamp. A failed progress write is still
// returned; any other failure keeps its old handling and spends the week.
func TestStoryPointsWeeklyBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		adoption  bool // the week-after check is the step the budget ends
		badBeads  bool // the retrain fails on a landing time that does not parse
		failWrite bool // the progress line cannot be written
		wireWait  bool // the town's wire is the step the budget ends
	}{
		{"retrain waits", false, false, false, false},
		{"adoption check waits", true, false, false, false},
		{"the wire waits", false, false, false, true},
		{"progress write failure is returned", false, false, true, false},
		{"another failure spends the week", false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			beads := trainTestBeads(90, 1, true)
			if tc.badBeads {
				beads = badTimeBeads()
			}
			fix := newTrainTestFixture(t, beads)
			last := weeklyNow.AddDate(0, 0, -8)
			writeWeeklyLastRun(t, fix.town, last)
			if tc.adoption {
				writeAt(t, adoptRuntimePath(fix.town), adoptTestWeights{boxes: 0.1, intercept: 1}.JSON())
				writeAt(t, storyPointsAdoptionsPath(fix.town), fmt.Sprintf("{\"event\":\"adopted\",\"at\":%q,\"version\":\"v1\",\"previous\":\"\"}\n",
					weeklyNow.Add(-storyPointsCheckAge-24*time.Hour).Format(time.RFC3339)))
			}
			var emb storypoints.Embedder = budgetBlockingEmbedder{}
			if tc.badBeads {
				emb = trainTestEmbedder{}
			}
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), emb)
			// The bad-beads case does real work, so it keeps the default budget:
			// its failure is the landing time, never a wall clock.
			if !tc.badBeads {
				c.Budget = 5 * time.Millisecond
			}
			if tc.wireWait {
				c.Wire = func(ctx context.Context) (RealTown, error) { <-ctx.Done(); return RealTown{}, ctx.Err() }
			}
			if tc.failWrite {
				c.Progress = failingWriter{}
			}

			err := c.RunStoryPointsWeekly(context.Background())
			got := out.String()

			switch {
			case tc.badBeads:
				if err == nil || !strings.HasPrefix(got, "story points weekly: failed: ") || !strings.Contains(got, "last tuesday") || !strings.Contains(got, "the week is spent") {
					t.Fatalf("err = %v, out = %q, want the failure, its cause and that the week is spent", err, got)
				}
				if want := weeklyNow.UTC().Format(time.RFC3339); weeklyLastRun(t, fix.town) != want {
					t.Errorf("stamp = %q, want the week recorded as spent at %q", weeklyLastRun(t, fix.town), want)
				}
			case tc.failWrite:
				if err == nil || !strings.Contains(err.Error(), "closed pipe") || !strings.HasPrefix(got, "story points weekly: waiting: ") {
					t.Fatalf("err = %v, out = %q, want the wait's line and the progress failure returned", err, got)
				}
			default:
				if err != nil {
					t.Fatalf("RunStoryPointsWeekly err = %v, want nil: the budget's end is a wait", err)
				}
				if !strings.HasPrefix(got, "story points weekly: waiting: the 5ms budget ended") || !strings.Contains(got, "the next hour continues from the embedding cache") {
					t.Errorf("out = %q, want the wait naming the budget and the cache", got)
				}
				if stamp := weeklyLastRun(t, fix.town); stamp != last.Format(time.RFC3339) {
					t.Errorf("stamp = %q, want the week left open at %q", stamp, last.Format(time.RFC3339))
				}
				if !tc.adoption {
					requireNoWeeklyWeights(t, fix.town)
				}
			}
		})
	}
}
