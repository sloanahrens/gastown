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
// failed — here on an embedder that cannot be reached — is on the line, and the
// week is not spent, so the next hour tries again.
func TestStoryPointsWeeklyReportsAFailedRetrainAndLeavesTheWeek(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{fail: true})
	if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("RunStoryPointsWeekly: %v", err)
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
