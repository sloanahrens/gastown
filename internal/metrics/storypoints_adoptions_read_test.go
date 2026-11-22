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

// TestReadStoryPointsAdoptions: the adoptions log reads back in order with the check's fields;
// no file is no adoption, and a log that cannot be read or does not parse is an error naming the
// path, never an empty log (gt-q2rrvb, the reader of the week-after check).
func TestReadStoryPointsAdoptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "adoptions.jsonl")
	env := Env{ReadFile: os.ReadFile}

	t.Run("an absent log is no adoption", func(t *testing.T) {
		t.Parallel()
		got, err := readStoryPointsAdoptions(env, filepath.Join(dir, "absent.jsonl"))
		if err != nil || len(got) != 0 {
			t.Errorf("read = %v, %v; want no adoption and no error", got, err)
		}
	})

	t.Run("an adopted and a checked line read back in order", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "adoptions.jsonl")
		body := `{"event":"adopted","at":"2026-11-10T06:00:00Z","version":"v2","previous":"weights.v1.json"}` + "\n" +
			`{"event":"checked","at":"2026-11-17T06:00:00Z","adopted_at":"2026-11-10T06:00:00Z","outcome":"reverted","reason":"worse"}` + "\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := readStoryPointsAdoptions(env, p)
		if err != nil || len(got) != 2 {
			t.Fatalf("read = %v, %v; want two lines", got, err)
		}
		adoptedAt := time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC)
		if a := got[0]; a.Event != "adopted" || !a.At.Equal(adoptedAt) || a.Version != "v2" || a.Previous != "weights.v1.json" {
			t.Errorf("first = %+v, want the adoption", a)
		}
		if c := got[1]; c.Event != "checked" || c.Adopted == nil || !c.Adopted.Equal(adoptedAt) || c.Outcome != "reverted" || c.Reason != "worse" {
			t.Errorf("second = %+v, want the check with its adoption time, outcome and reason", c)
		}
	})

	t.Run("an unreadable file is an error naming the path", func(t *testing.T) {
		t.Parallel()
		failing := Env{ReadFile: func(string) ([]byte, error) { return nil, errors.New("disk gone") }}
		_, err := readStoryPointsAdoptions(failing, path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "disk gone") {
			t.Errorf("err = %v, want the path and the cause", err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, must not read as an absent log", err)
		}
	})

	t.Run("a malformed line is an error naming the path", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "adoptions.jsonl")
		if err := os.WriteFile(p, []byte(`{"event":"adopted","at":"2026-11-10T06:00:00Z"}`+"\n{not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readStoryPointsAdoptions(env, p)
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("err = %v, want an error naming %s", err, p)
		}
	})
}

// TestStoryPointsAutoRevertUnreadable: an adoptions log the weekly run cannot read, or whose
// newest line does not parse, fails the run and names the file in the receipt. No revert runs,
// the runtime weights and their version stay as they were, nothing is recorded as checked, and
// no retrain runs after it (gt-xrhl7d).
func TestStoryPointsAutoRevertUnreadable(t *testing.T) {
	t.Parallel()
	good := adoptTestWeights{boxes: 0.25, intercept: 3.5}.JSON()
	worse := adoptTestWeights{boxes: 0.25, intercept: 2.9}.JSON()
	adopted := fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":"weights.v1.json"}`+"\n",
		autoRevertCheckAdoptedAt.Format(time.RFC3339))

	for _, tc := range []struct {
		name       string
		unreadable bool // the log is a directory, so reading it fails
		log        string
	}{
		{"an unreadable log", true, ""},
		{"a newest line that does not parse", false, adopted + "{not json\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			writeAt(t, adoptVersionPath(fix.town, 1), good)
			writeAt(t, adoptRuntimePath(fix.town), worse)
			logPath := storyPointsAdoptionsPath(fix.town)
			if tc.unreadable {
				if err := os.MkdirAll(logPath, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				writeAt(t, logPath, tc.log)
			}
			writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
			var out strings.Builder
			c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})

			err := c.RunStoryPointsWeekly(context.Background())

			if err == nil || !strings.Contains(err.Error(), logPath) {
				t.Fatalf("err = %v, want the adoptions log %s named", err, logPath)
			}
			if !strings.Contains(out.String(), logPath) {
				t.Errorf("receipt = %q, want the adoptions log named in it", out.String())
			}
			if got := readAt(t, adoptRuntimePath(fix.town)); got != worse {
				t.Errorf("runtime weights = %q, want the adopted weights left in place", got)
			}
			if got := readAt(t, adoptVersionPath(fix.town, 1)); got != good {
				t.Errorf("version 1 = %q, want it left in place", got)
			}
			if _, serr := os.Stat(adoptVersionPath(fix.town, 2)); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("version 2 stat = %v, want no version kept by a revert", serr)
			}
			if !tc.unreadable && readAt(t, logPath) != tc.log {
				t.Errorf("log = %q, want it unchanged: no check recorded", readAt(t, logPath))
			}
			if _, serr := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); !errors.Is(serr, fs.ErrNotExist) {
				t.Errorf("proposal stat = %v, want no retrain after a failed check", serr)
			}
		})
	}
}
