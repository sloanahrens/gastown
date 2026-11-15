package metrics

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// weeklyCommands wires the fixture's town for the weekly run: the checked-in
// weights the case names, the embedder and bead store the fixture holds, and the
// fixed clock.
func weeklyCommands(t *testing.T, fix trainTestFixture, out io.Writer, now time.Time, base storypoints.Weights, emb storypoints.Embedder) Commands {
	t.Helper()
	return Commands{
		Today: commandToday,
		Town:  fix.town,
		Wire: func(context.Context) (RealTown, error) {
			return RealTown{Town: fix.town, Extract: ExtractEnv{OutDir: fix.outDir}}, nil
		},
		Env: Env{
			Town:     fix.town,
			ReadFile: os.ReadFile,
			ReadDir:  listDir,
			Beads:    func(context.Context, string) BeadStore { return fix.store() },
		},
		Out:             out,
		StoryPointsBase: &base,
		Embedder:        emb,
		Now:             func() time.Time { return now },
	}
}

// weeklyBase is the weights in use before the weekly run: a shape ridge that
// spreads, which is what the adoption rule compares the proposal against.
func weeklyBase(t *testing.T) storypoints.Weights {
	t.Helper()
	return adoptWeights(t, adoptTestWeights{boxes: 0.1, intercept: 1})
}

// weeklyNow is the clock every case reads.
var weeklyNow = time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)

// writeWeeklyLastRun records the time the weekly run last decided, the way the
// run itself records it.
func writeWeeklyLastRun(t *testing.T, town string, at time.Time) {
	t.Helper()
	writeAt(t, storyPointsWeeklyStampPath(town), at.UTC().Format(time.RFC3339)+"\n")
}

// weeklyLastRun is what the run recorded: the time it decided.
func weeklyLastRun(t *testing.T, town string) string {
	t.Helper()
	return strings.TrimSpace(readAt(t, storyPointsWeeklyStampPath(town)))
}

// requireNoWeeklyWeights fails the case when the run wrote a runtime file.
func requireNoWeeklyWeights(t *testing.T, town string) {
	t.Helper()
	if _, err := os.Stat(adoptRuntimePath(town)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a runtime weights file exists after a run that adopted nothing (stat err = %v)", err)
	}
}

// requireWeeklyProposal fails the case unless the retrain left a proposed file
// under one of its two names.
func requireWeeklyProposal(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{storyPointsProposedName, storyPointsProposedName + storyPointsRejectedSuffix} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
	}
	t.Fatalf("no proposed weights under %s", dir)
}

// TestStoryPointsWeeklyRunsTheRetrainAndTheAdoption: a run more than a week after
// the last decision retrains, adopts the proposal the rule accepts, writes the
// outcome on one line, and records the run as the week's last.
func TestStoryPointsWeeklyRunsTheRetrainAndTheAdoption(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("RunStoryPointsWeekly: %v", err)
	}
	requireWeeklyProposal(t, fix.outDir)
	requireFile(t, adoptRuntimePath(fix.town))
	got := out.String()
	if !strings.HasPrefix(got, "story points weekly: adopted: ") {
		t.Errorf("out = %q, want the adopted decision the rule made", got)
	}
	if !strings.Contains(got, "wrote "+adoptRuntimePath(fix.town)) {
		t.Errorf("out = %q, want it to name the file the adoption wrote", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("out holds %d lines, want the run's one line", n)
	}
	if stamp := weeklyLastRun(t, fix.town); stamp != weeklyNow.Format(time.RFC3339) {
		t.Errorf("last run = %q, want the run that decided, %s", stamp, weeklyNow.Format(time.RFC3339))
	}
}

// TestStoryPointsWeeklyDoesNothingInsideTheWeek: the run inside the week reads
// nothing, writes nothing but its line, and leaves the last run where it was.
func TestStoryPointsWeeklyDoesNothingInsideTheWeek(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	last := weeklyNow.Add(-time.Hour)
	writeWeeklyLastRun(t, fix.town, last)
	var out strings.Builder
	// A store and an embedder that fail say the run read neither.
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{fail: true})
	c.Env.Beads = func(context.Context, string) BeadStore {
		return trainTestStore{err: errors.New("the bead store must not be read inside the week")}
	}
	if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("RunStoryPointsWeekly: %v", err)
	}
	if got, want := out.String(), "story points weekly: not due, the last run is under 168h0m0s old\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a proposed file was written inside the week (stat err = %v)", err)
	}
	requireNoWeeklyWeights(t, fix.town)
	if stamp := weeklyLastRun(t, fix.town); stamp != last.Format(time.RFC3339) {
		t.Errorf("last run = %q, want the run inside the week to leave %s in place", stamp, last.Format(time.RFC3339))
	}
}

// TestStoryPointsWeeklyReportsARetrainThatCannotFit: too few rows is the retrain's
// skip, not a failure: the run says so, adopts nothing, and spends the week so
// the town is not retrained with every hour until the history is long enough.
func TestStoryPointsWeeklyReportsARetrainThatCannotFit(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(10, 1, true))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("RunStoryPointsWeekly: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "story points weekly: skipped: ") || !strings.Contains(got, "too few samples") {
		t.Errorf("out = %q, want the skip and the retrain's own reason", got)
	}
	requireNoWeeklyWeights(t, fix.town)
	if stamp := weeklyLastRun(t, fix.town); stamp != weeklyNow.Format(time.RFC3339) {
		t.Errorf("last run = %q, want the skip to be the week's run, %s", stamp, weeklyNow.Format(time.RFC3339))
	}
}

// TestStoryPointsWeeklyReportsAFailedRetrainAndLeavesTheWeek: a retrain that
// failed — here on an embedder that cannot be reached — is on the line and is
// returned as the run's error, so the hour exits non-zero, and the week is not
// spent, so the next hour tries again.
func TestStoryPointsWeeklyReportsAFailedRetrainAndLeavesTheWeek(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{fail: true})
	err := c.RunStoryPointsWeekly(context.Background())
	if err == nil || !strings.Contains(err.Error(), "embedder unreachable") {
		t.Fatalf("RunStoryPointsWeekly err = %v, want the failed retrain returned", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "story points weekly: failed: ") || !strings.Contains(got, "embedder unreachable") {
		t.Errorf("out = %q, want the failure and its cause", got)
	}
	if !strings.Contains(got, "the next hour tries again") {
		t.Errorf("out = %q, want it to say the week is not spent", got)
	}
	requireNoWeeklyWeights(t, fix.town)
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed run recorded the week (stat err = %v)", err)
	}
}

// TestStoryPointsWeeklySpendsTheWeekOnAFailureNoHourCanMeetDifferently: a failure
// that is neither a missing extract nor the embedder (here a landing time that
// does not parse) is on the line with its error, is returned so the hour exits
// non-zero, and spends the week: the stamp is recorded, so the next hour's guard
// answers without wiring the town.
func TestStoryPointsWeeklySpendsTheWeekOnAFailureNoHourCanMeetDifferently(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, badTimeBeads())
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})

	err := c.RunStoryPointsWeekly(context.Background())

	if err == nil {
		t.Fatal("RunStoryPointsWeekly err = nil, want the failure returned so the hour exits non-zero")
	}
	got := out.String()
	if !strings.HasPrefix(got, "story points weekly: failed: ") || !strings.Contains(got, "last tuesday") {
		t.Errorf("out = %q, want the failure and its cause (the landing time that did not parse)", got)
	}
	if !strings.Contains(got, "the week is spent") {
		t.Errorf("out = %q, want it to say the week is spent", got)
	}
	if want := weeklyNow.UTC().Format(time.RFC3339); weeklyLastRun(t, fix.town) != want {
		t.Errorf("stamp = %q, want the run recorded as the week's last, %q", weeklyLastRun(t, fix.town), want)
	}
	requireNoWeeklyWeights(t, fix.town)
}

// previousExtractDir is the fixture's metrics root joined to the day before its
// extract's day.
func previousExtractDir(t *testing.T, fix trainTestFixture) string {
	t.Helper()
	today, err := time.Parse(time.DateOnly, commandToday)
	if err != nil {
		t.Fatalf("commandToday %q is not a date: %v", commandToday, err)
	}
	return filepath.Join(filepath.Dir(fix.outDir), today.AddDate(0, 0, -1).Format(time.DateOnly))
}

// layOutPreviousExtract writes the landings table of the day before the
// fixture's extract under its metrics root: the extract ran yesterday, so
// today's table is merely not due yet. Only the table's presence is read.
func layOutPreviousExtract(t *testing.T, fix trainTestFixture) {
	t.Helper()
	writeAt(t, filepath.Join(previousExtractDir(t, fix), landingsName),
		strings.Join(landingColumns, ",")+"\n")
}

// TestStoryPointsWeeklyWaitsForTheDaysExtract: no landings table for the day yet
// is the extract's lateness, not a fault, while yesterday's extract is under the
// metrics root: the run writes its wait line saying the next hour tries again,
// leaves the week unspent, and returns nil so the hour exits 0.
func TestStoryPointsWeeklyWaitsForTheDaysExtract(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	if err := os.RemoveAll(fix.outDir); err != nil {
		t.Fatal(err)
	}
	layOutPreviousExtract(t, fix)
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})

	err := c.RunStoryPointsWeekly(context.Background())

	if err != nil {
		t.Fatalf("RunStoryPointsWeekly err = %v, want nil: a missing extract is the wait for it", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "story points weekly: waiting: ") || !strings.Contains(got, "run extract first") {
		t.Errorf("out = %q, want the wait and the step that writes the table", got)
	}
	if !strings.Contains(got, "the next hour tries again") {
		t.Errorf("out = %q, want it to say the next hour tries again", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("out holds %d lines, want the run's one line", n)
	}
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing extract spent the week (stat err = %v)", err)
	}
}

// TestStoryPointsWeeklyFailsWhenTheExtractHasNotRunForADay: no extract for the
// day nor for the day before is not the day's lateness but an extract that has
// stopped: the run writes its line, returns the error so the hour exits non-zero
// and the daemon escalates, and leaves the week unspent.
func TestStoryPointsWeeklyFailsWhenTheExtractHasNotRunForADay(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	if err := os.RemoveAll(fix.outDir); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})

	err := c.RunStoryPointsWeekly(context.Background())

	if err == nil || !strings.Contains(err.Error(), "run extract first") {
		t.Fatalf("RunStoryPointsWeekly err = %v, want the missing extract returned after a day", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "story points weekly: failed: ") || !strings.Contains(got, "the previous day has no extract either") {
		t.Errorf("out = %q, want the failure and why the wait is over", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("out holds %d lines, want the run's one line", n)
	}
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing extract spent the week (stat err = %v)", err)
	}
}

// TestStoryPointsWeeklyFailsWhenThePreviousDayCannotBeRead: a previous day's
// table that cannot be read is not a day the extract did not run, and it is not
// counted as one: the run returns the error and leaves the week unspent.
func TestStoryPointsWeeklyFailsWhenThePreviousDayCannotBeRead(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	if err := os.RemoveAll(fix.outDir); err != nil {
		t.Fatal(err)
	}
	// A directory where the table goes reads as an error, not as an absent table.
	if err := os.MkdirAll(filepath.Join(previousExtractDir(t, fix), landingsName), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})

	err := c.RunStoryPointsWeekly(context.Background())

	if err == nil || !strings.Contains(err.Error(), "the previous day's extract") {
		t.Fatalf("RunStoryPointsWeekly err = %v, want the unreadable previous day returned", err)
	}
	if got := out.String(); !strings.Contains(got, "could not be checked") {
		t.Errorf("out = %q, want it to say the previous day could not be checked", got)
	}
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unreadable previous day spent the week (stat err = %v)", err)
	}
}

// TestStoryPointsWeeklyFailsWhenTheWaitLineIsNotWritten: the wait's line is all
// the hour reports, so a line that cannot be written fails the run rather than
// exiting 0 over a wait nobody was told about.
func TestStoryPointsWeeklyFailsWhenTheWaitLineIsNotWritten(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	if err := os.RemoveAll(fix.outDir); err != nil {
		t.Fatal(err)
	}
	layOutPreviousExtract(t, fix)
	c := weeklyCommands(t, fix, &strings.Builder{}, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	c.Out = failingWriter{}

	err := c.RunStoryPointsWeekly(context.Background())

	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("RunStoryPointsWeekly err = %v, want the failed line returned", err)
	}
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing extract spent the week (stat err = %v)", err)
	}
}

// TestStoryPointsWeeklyFailsWhenTheDayDirectoryIsNotADay: the wait's bound is
// the day before the day the wired extract directory names; a directory that
// names no day is an error, not a wait, because nothing says which table the day
// before it holds.
func TestStoryPointsWeeklyFailsWhenTheDayDirectoryIsNotADay(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	c.Wire = func(context.Context) (RealTown, error) {
		return RealTown{Town: fix.town, Extract: ExtractEnv{OutDir: filepath.Dir(fix.outDir)}}, nil
	}

	err := c.RunStoryPointsWeekly(context.Background())

	if err == nil || !strings.Contains(err.Error(), "is not named for a day") {
		t.Fatalf("RunStoryPointsWeekly err = %v, want the unnamed day directory returned", err)
	}
	if got := out.String(); !strings.Contains(got, "could not be checked") {
		t.Errorf("out = %q, want it to say the previous day could not be checked", got)
	}
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unnamed day directory spent the week (stat err = %v)", err)
	}
}

// TestStoryPointsWeeklyRetriesAnEmbedderTheNextHourAnswers: the first hour's
// embedder is unreachable and leaves the week open; the next hour, with the
// embedder answering, reaches a decision and spends the week.
func TestStoryPointsWeeklyRetriesAnEmbedderTheNextHourAnswers(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	var first strings.Builder
	down := weeklyCommands(t, fix, &first, weeklyNow, weeklyBase(t), trainTestEmbedder{fail: true})
	if err := down.RunStoryPointsWeekly(context.Background()); err == nil {
		t.Fatal("first hour err = nil, want the unreachable embedder returned")
	}
	if _, err := os.Stat(storyPointsWeeklyStampPath(fix.town)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the failed first hour spent the week (stat err = %v)", err)
	}

	var second strings.Builder
	up := weeklyCommands(t, fix, &second, weeklyNow.Add(time.Hour), weeklyBase(t), trainTestEmbedder{})
	if err := up.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("second hour err = %v, want the retry to decide", err)
	}
	if want := weeklyNow.Add(time.Hour).UTC().Format(time.RFC3339); weeklyLastRun(t, fix.town) != want {
		t.Errorf("stamp = %q, want the second hour recorded, %q", weeklyLastRun(t, fix.town), want)
	}
}

// TestStoryPointsWeeklyRefusals: a run that cannot read the guard, cannot reach
// its town, or cannot record its decision fails instead of retraining on a guess
// or adopting the same proposal again.
func TestStoryPointsWeeklyRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// fewRows gives the town fewer measured landings than a retrain fits, so
		// the case reaches the run's own recording of the week.
		fewRows bool
		// adopts says the case gets as far as writing the runtime weights, which a
		// case that refuses before its steps must leave alone.
		adopts bool
		// layOut prepares the town and returns the run.
		layOut func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands
		want   string
	}{
		{
			name: "the last run is a directory, so it cannot be read",
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				writeAt(t, filepath.Join(storyPointsWeeklyStampPath(fix.town), "not a file"), "x")
				return weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
			},
			want: "read the last run",
		},
		{
			name: "the last run does not hold a time",
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				writeAt(t, storyPointsWeeklyStampPath(fix.town), "last Tuesday\n")
				return weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
			},
			want: "does not hold an RFC3339 time",
		},
		{
			name: "no town was named",
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				c := weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
				c.Town = ""
				return c
			},
			want: "Commands.Town is unset",
		},
		{
			name: "the town cannot be wired",
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				c := weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
				c.Wire = func(context.Context) (RealTown, error) {
					return RealTown{}, errors.New("no workspace root")
				}
				return c
			},
			want: "story points weekly: no workspace root",
		},
		{
			name: "the wire named no town",
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				c := weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
				c.Wire = func(context.Context) (RealTown, error) { return RealTown{}, nil }
				return c
			},
			want: "the wired town is empty",
		},
		{
			name:    "the skip cannot be recorded",
			fewRows: true,
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				c := weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
				c.storyPointsStore.Write = func(string, string, func(io.Writer) error) error {
					return errors.New("the disk is full")
				}
				return c
			},
			want: "record the last run in",
		},
		{
			name:   "the adoption cannot be recorded",
			adopts: true,
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				c := weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
				// The weights land and the week does not: a full disk that took the
				// proposal is still a disk that cannot take the stamp.
				c.storyPointsStore.Write = func(dir, name string, write func(io.Writer) error) error {
					if name == storyPointsWeeklyStampName {
						return errors.New("the disk is full")
					}
					return writeAtomic(dir, name, write)
				}
				return c
			},
			want: "record the last run in",
		},
		{
			name:    "the last run has nowhere to be recorded",
			fewRows: true,
			layOut: func(t *testing.T, fix trainTestFixture, out *strings.Builder) Commands {
				// A dangling link where the story-points directory goes: the stamp reads
				// as no run yet, and the directory cannot be made to write one.
				if err := os.Symlink(filepath.Join(fix.town, "missing"), filepath.Join(fix.town, ".runtime", "storypoints")); err != nil {
					t.Fatalf("lay out a dangling story-points directory: %v", err)
				}
				return weeklyCommands(t, fix, out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
			},
			want: "create",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := 90
			if tc.fewRows {
				rows = 10
			}
			fix := newTrainTestFixture(t, trainTestBeads(rows, 1, true))
			var out strings.Builder
			err := tc.layOut(t, fix, &out).RunStoryPointsWeekly(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("out = %q, want no line from a run that refused", out.String())
			}
			if !tc.adopts {
				requireNoWeeklyWeights(t, fix.town)
			}
		})
	}
}
