package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestWeeklyAutoAdoptConfigIsReadOnlyWhenDue: the weekly run asks for story_points.auto_adopt
// only once its week guard says the retrain is due. A week that is not due never calls the
// reader, so a config that cannot be read leaves the hour green; a due week whose read fails
// stops once, naming the config and the error, and neither retrains nor stamps the run
// (gt-7ccgst).
func TestWeeklyAutoAdoptConfigIsReadOnlyWhenDue(t *testing.T) {
	t.Parallel()
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	broken := errors.New("patrol block does not parse")

	t.Run("not due: the reader is never called", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		calls := 0
		c.AutoAdoptReader = func() (bool, error) { calls++; return false, broken }
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v, want a green hour on a week that is not due", err)
		}
		if calls != 0 {
			t.Errorf("reader called %d times, want 0", calls)
		}
		if got := out.String(); !strings.Contains(got, "not due") {
			t.Errorf("out = %q, want the not-due receipt", got)
		}
	})

	t.Run("due and unreadable: one failure naming the config and the error", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptRuntimePath(fix.town), previous)
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		stamp := weeklyLastRun(t, fix.town)
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		calls := 0
		c.AutoAdoptReader = func() (bool, error) { calls++; return false, broken }
		err := c.RunStoryPointsWeekly(context.Background())
		if !errors.Is(err, broken) || !strings.Contains(err.Error(), "reading story_points.auto_adopt") {
			t.Fatalf("err = %v, want it to wrap the read error and name story_points.auto_adopt", err)
		}
		if calls != 1 {
			t.Errorf("reader called %d times, want once", calls)
		}
		requireNoRuntimeWrite(t, fix.town, previous)
		if got := weeklyLastRun(t, fix.town); got != stamp {
			t.Errorf("last run = %q, want it left at %q: the retrain did not run on a guess", got, stamp)
		}
	})

	t.Run("due and readable: called once and its answer decides", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptRuntimePath(fix.town), previous)
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		calls := 0
		c.AutoAdoptReader = func() (bool, error) { calls++; return true, nil }
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		if calls != 1 {
			t.Errorf("reader called %d times, want once", calls)
		}
		if got := out.String(); !strings.HasPrefix(got, "story points weekly: adopted: both rules pass: ") {
			t.Errorf("out = %q, want the stronger rule's decision from a reader answering on", got)
		}
	})
}

// TestWeeklyAutoAdoptConfigOffPathIsUnchanged: a nil reader, or one answering off, on a due week
// adopts by the rank rule as it did before the key existed (gt-7ccgst).
func TestWeeklyAutoAdoptConfigOffPathIsUnchanged(t *testing.T) {
	t.Parallel()
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	for name, reader := range map[string]func() (bool, error){
		"nil reader":    nil,
		"reader is off": func() (bool, error) { return false, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			writeAt(t, adoptRuntimePath(fix.town), previous)
			writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
			c.AutoAdoptReader = reader
			if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
				t.Fatalf("RunStoryPointsWeekly: %v", err)
			}
			if got := readAt(t, adoptRuntimePath(fix.town)); got == previous {
				t.Errorf("runtime weights unchanged: the weekly run must still adopt by the rank rule")
			}
		})
	}
}
