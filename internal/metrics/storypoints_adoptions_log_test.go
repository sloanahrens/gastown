package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readAdoptionsLog parses the adoptions log the weekly run wrote.
func readAdoptionsLog(t *testing.T, town string) []storyPointsAdoption {
	t.Helper()
	data, err := os.ReadFile(storyPointsAdoptionsPath(town))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read adoptions log: %v", err)
	}
	var log []storyPointsAdoption
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var a storyPointsAdoption
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("adoptions log line %q: %v", line, err)
		}
		log = append(log, a)
	}
	return log
}

// TestStoryPointsWeeklyLogsEachAdoption: each adoption the weekly run makes is logged with its
// time, the version it was kept as and the version file the weights it replaced were kept as,
// whichever rule adopted it: the key on (the stronger rule) and the key off (the rank rule, as
// before the key existed) both adopt and both log; a run that adopts nothing logs nothing, and
// the run's own outcome is unchanged (gt-q2rrvb).
func TestStoryPointsWeeklyLogsEachAdoption(t *testing.T) {
	t.Parallel()
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	for _, tc := range []struct {
		name string
		on   bool
	}{{"key on adopts and logs", true}, {"key off adopts by the rank rule as before and logs", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			writeAt(t, adoptRuntimePath(fix.town), previous)
			writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
			c.StoryPointsAutoAdopt = tc.on
			if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
				t.Fatalf("RunStoryPointsWeekly: %v", err)
			}
			if got := readAt(t, adoptRuntimePath(fix.town)); got == previous {
				t.Errorf("runtime weights unchanged: the weekly run must still adopt")
			}
			log := readAdoptionsLog(t, fix.town)
			if len(log) != 1 {
				t.Fatalf("adoptions log = %+v, want one adoption", log)
			}
			if a := log[0]; a.Event != "adopted" || !a.At.Equal(weeklyNow) || a.Version == "" || a.Previous != "weights.v1.json" {
				t.Errorf("logged = %+v, want an adoption at %s kept as a version, replacing weights.v1.json", a, weeklyNow)
			}
		})
	}

	t.Run("a run that adopts nothing logs nothing", func(t *testing.T) {
		t.Parallel()
		// A proposal that does not rank the holdout beads is rejected, and nothing is written.
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, false))
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		c.StoryPointsAutoAdopt = true
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		if log := readAdoptionsLog(t, fix.town); len(log) != 0 {
			t.Errorf("adoptions log = %+v, want none for a run that adopted nothing", log)
		}
	})
}

// TestStoryPointsAdoptionLogFailures: an adoption the log cannot record fails the weekly run
// naming that the adoption is not checked, and the weights it wrote stay; the append's own
// failures are each an error: an unreadable log, a time the log cannot encode, a directory it
// cannot create and a store that cannot write (gt-q2rrvb).
func TestStoryPointsAdoptionLogFailures(t *testing.T) {
	t.Parallel()
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()

	t.Run("a log that cannot be appended to fails the run and keeps the adoption", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptRuntimePath(fix.town), previous)
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		// The week-after check reads the log first and finds none; the append's own read of it fails.
		c.Env.ReadFile = failNthRead("adoptions.jsonl", 2)
		err := c.RunStoryPointsWeekly(context.Background())
		if err == nil || !strings.Contains(err.Error(), "is not logged, so it is not checked") {
			t.Fatalf("err = %v, want the unlogged adoption named", err)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got == previous {
			t.Errorf("runtime weights unchanged: the adoption was written before the log failed")
		}
	})

	t.Run("the append's own errors", func(t *testing.T) {
		t.Parallel()
		var c Commands
		env := Env{ReadFile: os.ReadFile}
		good := storyPointsAdoption{Event: "adopted", At: weeklyNow, Version: "v1"}

		unencodable := good
		unencodable.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := c.appendStoryPointsAdoption(env, filepath.Join(t.TempDir(), "adoptions.jsonl"), unencodable); err == nil {
			t.Error("a time the log cannot encode: err = nil, want an error")
		}

		// ReadFile answers "absent" while the filesystem refuses the write: the log's directory
		// would sit under a regular file, and the log's name is taken by a directory.
		absent := Env{ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist }}
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.appendStoryPointsAdoption(absent, filepath.Join(blocker, "sub", "adoptions.jsonl"), good); err == nil || !strings.Contains(err.Error(), "create") {
			t.Errorf("a directory it cannot create: err = %v, want a create error", err)
		}
		taken := t.TempDir()
		if err := os.MkdirAll(filepath.Join(taken, "adoptions.jsonl"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := c.appendStoryPointsAdoption(absent, filepath.Join(taken, "adoptions.jsonl"), good); err == nil {
			t.Error("a store that cannot write: err = nil, want an error")
		}
		failRead := Env{ReadFile: func(string) ([]byte, error) { return nil, errors.New("disk gone") }}
		if err := c.appendStoryPointsAdoption(failRead, filepath.Join(t.TempDir(), "adoptions.jsonl"), good); err == nil || !strings.Contains(err.Error(), "disk gone") {
			t.Errorf("an unreadable prior log: err = %v, want the read error", err)
		}
	})
}
