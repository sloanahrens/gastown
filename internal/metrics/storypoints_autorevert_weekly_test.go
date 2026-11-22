package metrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWeeklyRunChecksTheLastAdoption: the weekly run judges the last adoption before it
// retrains, so an adoption that did worse is reverted on the next weekly run: whichever rule made
// it (the key off is the rank rule, as before the key existed, the key on the stronger rule), and
// whether it replaced earlier runtime weights or the checked-in ones (gt-q2rrvb).
func TestWeeklyRunChecksTheLastAdoption(t *testing.T) {
	t.Parallel()
	good := adoptTestWeights{boxes: 0.25, intercept: 3.5}.JSON()
	worse := adoptTestWeights{boxes: 0.25, intercept: 2.9}.JSON()
	adoptedAt := autoRevertCheckAdoptedAt

	for _, tc := range []struct {
		name  string
		on    bool
		first bool // the adoption replaced the checked-in weights
	}{
		{"key off, an adoption that replaced runtime weights", false, false},
		{"key on, an adoption that replaced runtime weights", true, false},
		{"key off, a first adoption", false, true},
		{"key on, a first adoption", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			adopted, prevName := worse, "weights.v1.json"
			writeAt(t, adoptVersionPath(fix.town, 1), good)
			if tc.first {
				adopted, prevName = firstWorse, ""
				if err := os.Remove(adoptVersionPath(fix.town, 1)); err != nil {
					t.Fatal(err)
				}
			}
			writeAt(t, adoptRuntimePath(fix.town), adopted)
			writeAt(t, storyPointsAdoptionsPath(fix.town), fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":%q}`+"\n",
				adoptedAt.Format(time.RFC3339), prevName))
			writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
			c.StoryPointsAutoAdopt = tc.on

			if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
				t.Fatalf("RunStoryPointsWeekly: %v", err)
			}

			log := readAdoptionsLog(t, fix.town)
			if len(log) < 2 || log[1].Event != "checked" || log[1].Outcome != "reverted" {
				t.Fatalf("log = %+v, want the adoption, then its check recorded as reverted before anything the retrain adopts", log)
			}
			if tc.first {
				if got := readAt(t, adoptVersionPath(fix.town, 1)); got != firstWorse {
					t.Errorf("version 1 = %q, want the reverted first adoption kept", got)
				}
			} else if got := readAt(t, adoptVersionPath(fix.town, 1)); got == "" {
				t.Errorf("version 1 is empty")
			}
			requireWeeklyProposal(t, fix.outDir)
		})
	}
}

// TestWeeklyRunFailsWhenTheAdoptionCheckFails: a check that cannot decide fails the weekly run
// through weeklyFailure: the error names the cause, the retrain does not run, and the week is
// spent like any other failure of the run (gt-q2rrvb).
func TestWeeklyRunFailsWhenTheAdoptionCheckFails(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
	writeAt(t, storyPointsAdoptionsPath(fix.town), "{not json\n")
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})

	err := c.RunStoryPointsWeekly(context.Background())

	if err == nil || !strings.Contains(err.Error(), "adoptions.jsonl") {
		t.Fatalf("err = %v, want the unreadable adoptions log named", err)
	}
	if !strings.Contains(out.String(), "the week is spent") {
		t.Errorf("out = %q, want the failure line saying the week is spent", out.String())
	}
	if _, serr := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("proposal stat = %v, want no retrain after a failed check", serr)
	}
	if stamp := weeklyLastRun(t, fix.town); stamp != weeklyNow.Format(time.RFC3339) {
		t.Errorf("last run = %q, want the failed week recorded as spent at %s", stamp, weeklyNow.Format(time.RFC3339))
	}
}
