package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// unrecordedRevertLog is an adoptions log holding one adoption and no check, the
// state a revert leaves when the process died, or the append failed, between the
// revert and the record (gt-skeyaw). previous is the log's Previous.
func unrecordedRevertLog(previous string) string {
	return fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":%q}`+"\n",
		autoRevertCheckAdoptedAt.Format(time.RFC3339), previous)
}

// TestWeeklyRunRecordsAnUnrecordedRevert: an adoption whose revert happened but whose
// check line was never written is recorded as reverted, in both states a revert leaves,
// and the week goes on to the retrain; any other missing file stays the loud failure
// it was (gt-skeyaw).
func TestWeeklyRunRecordsAnUnrecordedRevert(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	due := autoRevertCheckAdoptedAt.Add(7 * day)
	good := adoptTestWeights{boxes: 0.25, intercept: 3.5}.JSON()

	// A first adoption's revert kept the adopted weights as version 1 and removed the
	// runtime file; a later adoption's revert renamed version 1 over the runtime file.
	firstAdoptionReverted := autoRevertCheckCase{now: due, log: unrecordedRevertLog(""),
		prepare: func(fix trainTestFixture) { writeAt(t, adoptVersionPath(fix.town, 1), firstWorse) }}
	laterAdoptionReverted := autoRevertCheckCase{adopted: good, now: due, log: unrecordedRevertLog("weights.v1.json")}

	for _, tc := range []struct {
		name     string
		c        autoRevertCheckCase
		inferred string // what the recorded reason says the revert was inferred from
	}{
		{"a first adoption whose revert was not recorded", firstAdoptionReverted,
			"inferred: the runtime weights file is missing and the newest version file carries the adopted version"},
		{"a later adoption whose revert was not recorded", laterAdoptionReverted,
			"inferred: the version file weights.v1.json is missing and the runtime weights file loads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix, out, err := runAutoRevertCheck(t, tc.c)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			log := readAdoptionsLog(t, fix.town)
			if len(log) != 2 || log[1].Event != "checked" || log[1].Outcome != "reverted" || !strings.Contains(log[1].Reason, "recorded after the revert") {
				t.Errorf("log = %+v, want the adoption, then a check recorded as reverted with a reason saying it was recorded after the revert", log)
			} else if !strings.Contains(log[1].Reason, tc.inferred) {
				t.Errorf("reason = %q, want it to say what the revert was inferred from: %q", log[1].Reason, tc.inferred)
			}
			if !strings.Contains(out, "found reverted") {
				t.Errorf("out = %q, want the receipt to say the adoption was found reverted", out)
			}
		})
	}

	t.Run("the week goes on to the retrain", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptVersionPath(fix.town, 1), firstWorse)
		writeAt(t, storyPointsAdoptionsPath(fix.town), unrecordedRevertLog(""))
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		if log := readAdoptionsLog(t, fix.town); len(log) < 2 || log[1].Outcome != "reverted" {
			t.Errorf("log = %+v, want the check recorded as reverted", log)
		}
		requireWeeklyProposal(t, fix.outDir)
	})

	for _, tc := range []struct {
		name string
		c    autoRevertCheckCase
		want string
	}{
		{"no runtime file and no version file stays the loud failure", autoRevertCheckCase{now: due, log: unrecordedRevertLog("")}, "weights.json: no such file"},
		{"no runtime file, no version file and a named previous stays the loud failure", autoRevertCheckCase{now: due, log: unrecordedRevertLog("weights.v1.json")}, "weights.json: no such file"},
		{"a version file that does not load is a loud failure naming it", autoRevertCheckCase{now: due, log: unrecordedRevertLog(""),
			prepare: func(fix trainTestFixture) { writeAt(t, adoptVersionPath(fix.town, 1), "{not weights") }}, "weights.v1.json"},
		{"a version file of other weights than the adoption's is not read as its revert", autoRevertCheckCase{now: due, log: strings.Replace(unrecordedRevertLog(""), `"v1"`, `"v7"`, 1),
			prepare: func(fix trainTestFixture) { writeAt(t, adoptVersionPath(fix.town, 1), firstWorse) }}, "weights.json: no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix, _, err := runAutoRevertCheck(t, tc.c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if log := readAt(t, storyPointsAdoptionsPath(fix.town)); strings.Contains(log, `"checked"`) {
				t.Errorf("log = %q, want no check recorded for a state it cannot classify", log)
			}
		})
	}

	t.Run("version files that cannot be listed are a loud failure", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, storyPointsAdoptionsPath(fix.town), unrecordedRevertLog(""))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, due, weeklyBase(t), trainTestEmbedder{})
		c.storyPointsStore = storyPointsStore{List: func(string) ([]DirEntry, error) { return nil, errors.New("directory gone") }}
		err := c.checkStoryPointsAdoption(context.Background(), RealTown{Town: fix.town, Extract: ExtractEnv{OutDir: fix.outDir}}, c.Env, trainTestEmbedder{}, due)
		if err == nil || !strings.Contains(err.Error(), "the version files cannot be listed") || !strings.Contains(err.Error(), "directory gone") {
			t.Errorf("err = %v, want it to say the version files cannot be listed and name the cause", err)
		}
	})

	t.Run("a found revert whose record cannot be written says so", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptVersionPath(fix.town, 1), firstWorse)
		writeAt(t, storyPointsAdoptionsPath(fix.town), unrecordedRevertLog(""))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, due, weeklyBase(t), trainTestEmbedder{})
		c.storyPointsStore = storyPointsStore{Write: func(string, string, func(io.Writer) error) error { return errors.New("disk gone") }}
		err := c.checkStoryPointsAdoption(context.Background(), RealTown{Town: fix.town, Extract: ExtractEnv{OutDir: fix.outDir}}, c.Env, trainTestEmbedder{}, due)
		if err == nil || !strings.Contains(err.Error(), "found reverted, but the check was not recorded") || !strings.Contains(err.Error(), "disk gone") {
			t.Errorf("err = %v, want it to say the revert was found but the check not recorded, and name the cause", err)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); strings.Contains(log, `"checked"`) {
			t.Errorf("log = %q, want no check recorded", log)
		}
	})
}
