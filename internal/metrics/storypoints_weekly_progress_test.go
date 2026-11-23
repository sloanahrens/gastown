package metrics

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestStoryPointsWeeklyProgress: after the train returns, failed or not, the run
// writes one line naming the cache's hits, misses and path; every miss is a call
// to the fake, including the one that failed (gt-tibj01).
func TestStoryPointsWeeklyProgress(t *testing.T) {
	t.Parallel()
	for _, fail := range []int{0, 1} {
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeWeeklyLastRun(t, fix.town, weeklyNow.AddDate(0, 0, -8))
		emb := &countingEmbedder{failAfter: fail}
		var out, progress strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), emb)
		c.Progress = &progress
		err := c.RunStoryPointsWeekly(context.Background())
		if (err != nil) != (fail > 0) {
			t.Fatalf("failAfter=%d: err = %v", fail, err)
		}
		got := progress.String()
		tail := fmt.Sprintf("misses=%d cache=%s\n", emb.calls+fail, weeklyCachePath(fix.town))
		if !strings.HasPrefix(got, "story points weekly: embeddings hits=") || !strings.HasSuffix(got, tail) {
			t.Errorf("failAfter=%d: progress = %q, want one line ending %q", fail, got, tail)
		}
		// A successful train asks for every row once, before the adoption.
		if want := fmt.Sprintf("story points weekly: embeddings hits=%d %s", len(fix.beads)-emb.calls, tail); fail == 0 && got != want {
			t.Errorf("progress = %q, want %q", got, want)
		}
	}
}
