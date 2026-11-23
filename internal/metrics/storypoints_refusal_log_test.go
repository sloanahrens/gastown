package metrics

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStoryPointsWeeklyLogsEachRefusal: the weekly run logs a week it did not adopt as one
// refused line carrying the reason of the rule that refused it — the rank rule with the key
// off, the stronger rule with it on — so the reader and the attention item see every weekly
// outcome. An adopted week logs no refused line, two refused weeks log two lines in the order
// they ran, and a refusal whose line cannot be appended fails the run naming the unlogged
// refusal (gt-7typlt).
func TestStoryPointsWeeklyLogsEachRefusal(t *testing.T) {
	t.Parallel()

	// refusedWeekLog runs a week the rule rejects and returns its adoptions log, failing the
	// case when the run errors, writes weights, or logs anything but one refusal at weeklyNow.
	refusedWeekLog := func(t *testing.T, on bool) []storyPointsAdoption {
		t.Helper()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, false))
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		c.StoryPointsAutoAdopt = on
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		requireNoWeeklyWeights(t, fix.town)
		log := readAdoptionsLog(t, fix.town)
		if len(log) != 1 {
			t.Fatalf("adoptions log = %+v, want one refusal", log)
		}
		if a := log[0]; a.Event != "refused" || !a.At.Equal(weeklyNow) {
			t.Errorf("logged = %+v, want a refusal at %s", a, weeklyNow)
		}
		return log
	}

	t.Run("with the key off the rank rule's reason is logged", func(t *testing.T) {
		t.Parallel()
		// The rank rule names no rule that failed: that prefix is the stronger rule's.
		if a := refusedWeekLog(t, false)[0]; !strings.HasPrefix(a.Reason, "rejected: ") || strings.Contains(a.Reason, "rule failed") {
			t.Errorf("reason = %q, want the rank rule's own reason", a.Reason)
		}
	})

	t.Run("with the key on the stronger rule's reason is logged", func(t *testing.T) {
		t.Parallel()
		// The stronger rule names the rule that failed and carries its reason.
		if a := refusedWeekLog(t, true)[0]; !strings.HasPrefix(a.Reason, "rejected: the rank rule failed: ") {
			t.Errorf("reason = %q, want the stronger rule's reason naming the rank rule", a.Reason)
		}
	})

	t.Run("an adopted week logs no refused line", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		c.StoryPointsAutoAdopt = true
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		log := readAdoptionsLog(t, fix.town)
		if len(log) != 1 || log[0].Event != "adopted" {
			t.Fatalf("adoptions log = %+v, want the week's adoption", log)
		}
		for _, a := range log {
			if a.Event == "refused" {
				t.Errorf("logged = %+v, want no refused line for a week that adopted", a)
			}
		}
	})

	t.Run("a refusal whose append fails fails the run naming the unlogged refusal", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, false))
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		c.StoryPointsAutoAdopt = true
		// The week-after check reads the log first; the append's own read of it fails.
		c.Env.ReadFile = failNthRead("adoptions.jsonl", 2)
		err := c.RunStoryPointsWeekly(context.Background())
		if err == nil || !strings.Contains(err.Error(), "the refusal is not logged") {
			t.Fatalf("err = %v, want the unlogged refusal named", err)
		}
		// The failure is not retryable, so the week is spent and nothing adopted.
		requireNoWeeklyWeights(t, fix.town)
		if got := weeklyLastRun(t, fix.town); got != weeklyNow.Format(time.RFC3339) {
			t.Errorf("weekly stamp = %q, want %s: the failed refusal's week is spent", got, weeklyNow.Format(time.RFC3339))
		}
	})

	t.Run("two refused weeks log two lines in order", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, false))
		next := weeklyNow.Add(8 * 24 * time.Hour)
		for _, at := range []time.Time{weeklyNow, next} {
			writeWeeklyLastRun(t, fix.town, at.Add(-8*24*time.Hour))
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, at, weeklyBase(t), trainTestEmbedder{})
			c.StoryPointsAutoAdopt = true
			if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
				t.Fatalf("RunStoryPointsWeekly at %s: %v", at, err)
			}
		}
		log := readAdoptionsLog(t, fix.town)
		if len(log) != 2 {
			t.Fatalf("adoptions log = %+v, want two refusals", log)
		}
		for i, want := range []time.Time{weeklyNow, next} {
			if a := log[i]; a.Event != "refused" || !a.At.Equal(want) || a.Reason == "" {
				t.Errorf("line %d = %+v, want a refusal at %s with its reason", i, a, want)
			}
		}
	})
}
