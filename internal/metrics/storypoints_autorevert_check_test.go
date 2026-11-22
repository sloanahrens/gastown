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

// autoRevertCheckAdoptedAt is when the adoption the check cases make was made: before the first
// landing of the fixture, so every fixture landing is since it.
var autoRevertCheckAdoptedAt = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

// autoRevertCheckCase is one run of the week-after check over the fixture's landings.
type autoRevertCheckCase struct {
	adopted, previous string    // the runtime weights, and the version 1 kept (empty: none)
	adoptedAt         time.Time // zero is autoRevertCheckAdoptedAt
	now               time.Time
	log               string                       // the log's line when set, else the adoption line
	readFile          func(string) ([]byte, error) // wraps the commands' reads when set
	prepare           func(fix trainTestFixture)
	firstAdoption     bool // the log's adoption replaced the checked-in weights (no previous version)
}

func runAutoRevertCheck(t *testing.T, tc autoRevertCheckCase) (trainTestFixture, string, error) {
	t.Helper()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	if tc.adopted != "" {
		writeAt(t, adoptRuntimePath(fix.town), tc.adopted)
	}
	prevName := ""
	if tc.previous != "" && !tc.firstAdoption {
		prevName = "weights.v1.json"
		writeAt(t, adoptVersionPath(fix.town, 1), tc.previous)
	}
	at := tc.adoptedAt
	if at.IsZero() {
		at = autoRevertCheckAdoptedAt
	}
	line := tc.log
	if line == "" {
		line = fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":%q}`+"\n", at.Format(time.RFC3339), prevName)
	}
	if line != "-" {
		writeAt(t, storyPointsAdoptionsPath(fix.town), line)
	}
	if tc.prepare != nil {
		tc.prepare(fix)
	}
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, tc.now, weeklyBase(t), trainTestEmbedder{})
	if tc.readFile != nil {
		c.Env.ReadFile = tc.readFile
	}
	err := c.checkStoryPointsAdoption(context.Background(), RealTown{Town: fix.town, Extract: ExtractEnv{OutDir: fix.outDir}}, c.Env, trainTestEmbedder{}, tc.now)
	return fix, out.String(), err
}

// failNthRead is a ReadFile that fails the nth read of a path ending in suffix and reads the
// rest from disk.
func failNthRead(suffix string, n int) func(string) ([]byte, error) {
	reads := 0
	return func(p string) ([]byte, error) {
		if strings.HasSuffix(p, suffix) {
			reads++
			if reads == n {
				return nil, errors.New("disk gone")
			}
		}
		return os.ReadFile(p)
	}
}

// firstWorse is runtime weights that do worse than the checked-in ones on the fixture.
var firstWorse = adoptTestWeights{boxes: -0.25, intercept: 1}.JSON()

// TestStoryPointsAutoRevertDecides: the week-after check of an automatic adoption reverts it
// when the weights it adopted do worse on the landings since and keeps it otherwise, recording
// either; an adoption not yet six days old is not checked (gt-q2rrvb).
func TestStoryPointsAutoRevertDecides(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	// Intercept 3.5 is the better of the two on the fixture's landings and 2.9 the worse, by
	// about 0.05 first-round rmse.
	good := adoptTestWeights{boxes: 0.25, intercept: 3.5}.JSON()
	worse := adoptTestWeights{boxes: 0.25, intercept: 2.9}.JSON()

	t.Run("a week old and worse by 0.05 rmse is reverted and recorded", func(t *testing.T) {
		t.Parallel()
		fix, out, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: worse, previous: good, now: autoRevertCheckAdoptedAt.Add(7 * day)})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got != good {
			t.Errorf("runtime weights = %q, want the weights it replaced", got)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); !strings.Contains(log, `"outcome":"reverted"`) {
			t.Errorf("log = %q, want the check recorded as reverted", log)
		}
		if !strings.Contains(out, "reverted") {
			t.Errorf("out = %q, want the receipt to say reverted", out)
		}
	})

	t.Run("a week old and better is kept and recorded", func(t *testing.T) {
		t.Parallel()
		fix, _, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: good, previous: worse, now: autoRevertCheckAdoptedAt.Add(7 * day)})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got != good {
			t.Errorf("runtime weights = %q, want the adopted weights kept", got)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); !strings.Contains(log, `"outcome":"kept"`) {
			t.Errorf("log = %q, want the check recorded as kept", log)
		}
	})

	t.Run("three days old is not checked", func(t *testing.T) {
		t.Parallel()
		fix, out, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: worse, previous: good, now: autoRevertCheckAdoptedAt.Add(3 * day)})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if out != "" {
			t.Errorf("out = %q, want no line for an adoption not yet a week old", out)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got != worse {
			t.Errorf("runtime weights changed to %q before the check was due", got)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); strings.Contains(log, `"checked"`) {
			t.Errorf("log = %q, want no check recorded", log)
		}
	})

	t.Run("a first adoption that did worse is reverted to the checked-in weights and kept as a version", func(t *testing.T) {
		t.Parallel()
		fix, _, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: firstWorse, now: autoRevertCheckAdoptedAt.Add(7 * day), firstAdoption: true})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if _, err := os.Stat(adoptRuntimePath(fix.town)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("runtime weights stat = %v, want the file removed so the checked-in weights are used", err)
		}
		if got := readAt(t, adoptVersionPath(fix.town, 1)); got != firstWorse {
			t.Errorf("version 1 = %q, want the adopted weights kept", got)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); !strings.Contains(log, `"outcome":"reverted"`) {
			t.Errorf("log = %q, want the check recorded as reverted", log)
		}
	})

	t.Run("a first adoption that did better is kept", func(t *testing.T) {
		t.Parallel()
		fix, _, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: good, now: autoRevertCheckAdoptedAt.Add(7 * day), firstAdoption: true})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got != good {
			t.Errorf("runtime weights = %q, want the adopted weights kept", got)
		}
	})

	t.Run("an adoption already checked is not checked again", func(t *testing.T) {
		t.Parallel()
		at := autoRevertCheckAdoptedAt
		logged := fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":"weights.v1.json"}`+"\n"+
			`{"event":"checked","at":%q,"adopted_at":%q,"outcome":"kept"}`+"\n",
			at.Format(time.RFC3339), at.Add(7*day).Format(time.RFC3339), at.Format(time.RFC3339))
		fix, out, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: worse, previous: good, now: at.Add(14 * day), log: logged})
		if err != nil || out != "" {
			t.Fatalf("check = %q, %v; want nothing for an adoption already checked", out, err)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got != worse {
			t.Errorf("runtime weights changed to %q by a second check", got)
		}
	})

	t.Run("a log with no adoption has nothing to check", func(t *testing.T) {
		t.Parallel()
		_, out, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: good, now: autoRevertCheckAdoptedAt.Add(7 * day), log: "-"})
		if err != nil || out != "" {
			t.Errorf("check = %q, %v; want nothing", out, err)
		}
	})
}

// TestStoryPointsAutoRevertFailures: each way the check cannot decide or act is an error that
// names its cause, and a check that waits records nothing (gt-q2rrvb).
func TestStoryPointsAutoRevertFailures(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	good := adoptTestWeights{boxes: 0.25, intercept: 3.5}.JSON()
	worse := adoptTestWeights{boxes: 0.25, intercept: 2.9}.JSON()
	due := autoRevertCheckAdoptedAt.Add(7 * day)

	for _, tc := range []struct {
		name string
		c    autoRevertCheckCase
		want string
	}{
		{"an unreadable log", autoRevertCheckCase{adopted: worse, previous: good, now: due,
			prepare: func(fix trainTestFixture) {
				_ = os.Remove(storyPointsAdoptionsPath(fix.town))
				if err := os.MkdirAll(storyPointsAdoptionsPath(fix.town), 0o755); err != nil {
					t.Fatal(err)
				}
			}}, "adoptions.jsonl"},
		{"a malformed log line", autoRevertCheckCase{adopted: worse, previous: good, now: due, log: "{not json\n"}, "adoptions.jsonl"},
		{"an extract without the columns", autoRevertCheckCase{adopted: worse, previous: good, now: due,
			prepare: func(fix trainTestFixture) { rewriteAutoAdoptLandings(t, fix, []string{"polecat_work"}, nil) }}, "polecat_work column"},
		{"no landings extract", autoRevertCheckCase{adopted: worse, previous: good, now: due,
			prepare: func(fix trainTestFixture) {
				if err := os.Remove(filepath.Join(fix.outDir, landingsName)); err != nil {
					t.Fatal(err)
				}
			}}, "first)"},
		{"a landings table that does not parse", autoRevertCheckCase{adopted: worse, previous: good, now: due,
			prepare: func(fix trainTestFixture) { writeAt(t, filepath.Join(fix.outDir, landingsName), "") }}, "empty file"},
		{"a landing whose time does not parse", autoRevertCheckCase{adopted: worse, previous: good, now: due,
			prepare: func(fix trainTestFixture) {
				rewriteAutoAdoptLandings(t, fix, nil, func(i int, row Row) {
					if i == 0 {
						row["landed_at"] = "not a time"
					}
				})
			}}, "bead "},
		{"runtime weights that do not load", autoRevertCheckCase{adopted: "{not weights", previous: good, now: due}, "weights.json:"},
		{"no runtime weights file", autoRevertCheckCase{previous: good, now: due}, "weights.json"},
		// A previous version file that is MISSING is a revert whose check was not recorded
		// (TestWeeklyRunRecordsAnUnrecordedRevert); one that is there and does not load is
		// the loud failure this row keeps (gt-skeyaw).
		{"an unreadable previous version", autoRevertCheckCase{adopted: worse, now: due,
			log:     fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":"weights.v7.json"}`+"\n", autoRevertCheckAdoptedAt.Format(time.RFC3339)),
			prepare: func(fix trainTestFixture) { writeAt(t, adoptVersionPath(fix.town, 7), "{not weights") }}, "weights.v7.json"},
		{"a previous version whose embedding the samples cannot use", autoRevertCheckCase{adopted: worse, previous: adoptTestWeights{boxes: 0.25, intercept: 3.5, dim: 5}.JSON(), now: due}, "weights.v1.json"},
		{"a revert that cannot read the version it restores", autoRevertCheckCase{adopted: worse, previous: good, now: due,
			readFile: failNthRead("weights.v1.json", 2)}, "could not be reverted"},
		{"a first adoption's revert that cannot read the runtime file again", autoRevertCheckCase{adopted: firstWorse, now: due, firstAdoption: true,
			readFile: failNthRead("weights.json", 2)}, "could not be reverted"},
		{"a first adoption's revert that cannot keep the adopted weights", autoRevertCheckCase{adopted: firstWorse, now: due, firstAdoption: true,
			prepare: func(fix trainTestFixture) {
				// A directory under the first numbered version's name: the version cannot be written.
				if err := os.MkdirAll(adoptVersionPath(fix.town, 1), 0o755); err != nil {
					t.Fatal(err)
				}
			}}, "could not be reverted"},
		{"a first adoption's revert that cannot remove the runtime file", autoRevertCheckCase{now: due, firstAdoption: true,
			readFile: func(p string) ([]byte, error) {
				if strings.HasSuffix(p, "weights.json") { // the file is not on disk: the read is made up
					return []byte(firstWorse), nil
				}
				return os.ReadFile(p)
			}}, "could not be reverted"},
		{"a previous version name with a path in it", autoRevertCheckCase{adopted: worse, now: due,
			log: fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":"../weights.json"}`+"\n", autoRevertCheckAdoptedAt.Format(time.RFC3339))}, "../weights.json"},
		{"a previous version name that is a directory dot", autoRevertCheckCase{adopted: worse, now: due,
			log: fmt.Sprintf(`{"event":"adopted","at":%q,"version":"v1","previous":".."}`+"\n", autoRevertCheckAdoptedAt.Format(time.RFC3339))}, "not a bare file name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := runAutoRevertCheck(t, tc.c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}

	t.Run("a check that cannot be recorded says what it did", func(t *testing.T) {
		t.Parallel()
		reads := 0
		fix, _, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: good, previous: worse, now: due,
			readFile: func(p string) ([]byte, error) {
				if strings.HasSuffix(p, "adoptions.jsonl") {
					reads++
					if reads > 1 { // the check's read passed; the append's read of the log fails
						return nil, errors.New("disk gone")
					}
				}
				return os.ReadFile(p)
			}})
		if err == nil || !strings.Contains(err.Error(), "kept, but the check was not recorded") {
			t.Errorf("err = %v, want it to say the adoption was kept but the check not recorded", err)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); strings.Contains(log, `"checked"`) {
			t.Errorf("log = %q, want no check recorded", log)
		}
	})

	t.Run("a holdout with no landings since the adoption waits and records nothing", func(t *testing.T) {
		t.Parallel()
		// The adoption is made after the last fixture landing, so no landing is since.
		fix, out, err := runAutoRevertCheck(t, autoRevertCheckCase{adopted: worse, previous: good,
			adoptedAt: autoRevertCheckAdoptedAt.Add(5 * day), now: autoRevertCheckAdoptedAt.Add(12 * day)})
		if err != nil || !strings.Contains(out, "waits") {
			t.Fatalf("check = %q, %v; want a receipt saying it waits", out, err)
		}
		if got := readAt(t, adoptRuntimePath(fix.town)); got != worse {
			t.Errorf("runtime weights changed to %q by a check that could not decide", got)
		}
		if log := readAt(t, storyPointsAdoptionsPath(fix.town)); strings.Contains(log, `"checked"`) {
			t.Errorf("log = %q, want the open check left unrecorded", log)
		}
	})
}
