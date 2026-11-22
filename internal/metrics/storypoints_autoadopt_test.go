package metrics

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// cellOr is cell when the case sets it, else def: a fixture bead's first-round
// cell takes the first-round value unless the case names another.
func cellOr(cell, def string) string {
	if cell == "" {
		return def
	}
	return cell
}

// rewriteAutoAdoptLandings rewrites the day's landings table with the columns in
// drop removed, and each row passed through edit by its index.
func rewriteAutoAdoptLandings(t *testing.T, fix trainTestFixture, drop []string, edit func(i int, row Row)) {
	t.Helper()
	path := filepath.Join(fix.outDir, landingsName)
	header, rows, err := ReadLandings(strings.NewReader(readAt(t, path)))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	var keep []string
	for _, col := range header {
		if !slices.Contains(drop, col) {
			keep = append(keep, col)
		}
	}
	for i, row := range rows {
		if edit != nil {
			edit(i, row)
		}
	}
	var b strings.Builder
	if err := WriteLandings(&b, keep, rows); err != nil {
		t.Fatalf("WriteLandings: %v", err)
	}
	writeAt(t, path, b.String())
}

// autoAdoptDecide runs the on-path adoption over the proposed file the case wrote,
// the decision the weekly run makes after its retrain. The case may change c's Env
// first. The rule is the stronger one, and the first-round flags come from the
// landing record.
func autoAdoptDecide(t *testing.T, c Commands, fix trainTestFixture) error {
	t.Helper()
	real := RealTown{Town: fix.town, Extract: ExtractEnv{OutDir: fix.outDir}}
	return c.decideStoryPoints(context.Background(), real, c.Env, storypoints.AdoptAuto, true)
}

// requireNoRuntimeWrite fails the case when the runtime weights changed from
// previous or a numbered version was written beside them.
func requireNoRuntimeWrite(t *testing.T, town, previous string) {
	t.Helper()
	if got := readAt(t, adoptRuntimePath(town)); got != previous {
		t.Errorf("runtime weights = %q, want the weights in use left as they were", got)
	}
	if _, err := os.Stat(adoptVersionPath(town, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("version 1 stat = %v, want no version written", err)
	}
}

// TestStoryPointsAutoAdoptAdopts: with story_points.auto_adopt on, a proposal that
// both rules pass becomes the runtime file and the weights it replaced are kept as
// version 1, and the weekly receipt names both. With the key off the same run
// adopts nothing, leaves the weights in use alone, and says so on its line.
func TestStoryPointsAutoAdoptAdopts(t *testing.T) {
	t.Parallel()
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()

	t.Run("key on adopts the passing proposal", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptRuntimePath(fix.town), previous)
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		c.StoryPointsAutoAdopt = true
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		proposed := readAt(t, filepath.Join(fix.outDir, storyPointsProposedName))
		if got := readAt(t, adoptRuntimePath(fix.town)); got != proposed {
			t.Errorf("runtime weights = %q, want the proposed file", got)
		}
		if got := readAt(t, adoptVersionPath(fix.town, 1)); got != previous {
			t.Errorf("version 1 = %q, want the weights it replaced", got)
		}
		got := out.String()
		if !strings.HasPrefix(got, "story points weekly: adopted: both rules pass: ") {
			t.Errorf("out = %q, want the decision both rules made", got)
		}
		for _, want := range []string{"wrote " + adoptRuntimePath(fix.town), "weights.v1.json"} {
			if !strings.Contains(got, want) {
				t.Errorf("out = %q, want it to name %q", got, want)
			}
		}
	})

	t.Run("key off adopts nothing", func(t *testing.T) {
		t.Parallel()
		fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
		writeAt(t, adoptRuntimePath(fix.town), previous)
		writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
		var out strings.Builder
		c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
		if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
			t.Fatalf("RunStoryPointsWeekly: %v", err)
		}
		requireWeeklyProposal(t, fix.outDir)
		requireNoRuntimeWrite(t, fix.town, previous)
		want := "story points weekly: not adopted: story_points.auto_adopt is off, so the proposed weights were written and nothing was adopted\n"
		if got := out.String(); got != want {
			t.Errorf("out = %q, want %q", got, want)
		}
		if stamp := weeklyLastRun(t, fix.town); stamp != weeklyNow.Format(time.RFC3339) {
			t.Errorf("last run = %q, want the run that decided, %s", stamp, weeklyNow.Format(time.RFC3339))
		}
	})
}

// TestStoryPointsAutoAdoptRefusesARankFailure: the weekly run with the key on names
// the rank rule as the failed rule when the proposal does not rank the holdout
// beads, and writes nothing to the runtime weights.
func TestStoryPointsAutoAdoptRefusesARankFailure(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, false))
	writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	c.StoryPointsAutoAdopt = true
	if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("RunStoryPointsWeekly: %v", err)
	}
	if !strings.HasPrefix(out.String(), "story points weekly: rejected: the rank rule failed: ") {
		t.Errorf("out = %q, want the rank rule named as the failure", out.String())
	}
	requireNoWeeklyWeights(t, fix.town)
}

// TestStoryPointsAutoAdoptRefuses: each input the stronger rule cannot decide from
// stops the adoption with the reason named in the step's receipt or its error, and
// writes no runtime file. A malformed or missing first-round column refuses before
// the holdout is built.
func TestStoryPointsAutoAdoptRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// setup edits the fixture's landings table or runtime weights before the step
		// runs. Nil leaves the table and the runtime file as the fixture wrote them.
		setup func(t *testing.T, fix trainTestFixture)
		// current, when set, is the runtime weights in use.
		current string
		// proposed is the proposed file the step reads.
		proposed string
		// wantErr is the substring of the error the step returns. Empty means the
		// step returns nil and wantOut names its receipt.
		wantErr string
		wantOut string
	}{
		{
			name:     "error rule rejects a proposal that ranks the same but predicts worse",
			current:  adoptTestWeights{boxes: 0.25, intercept: 5}.JSON(),
			proposed: adoptTestWeights{boxes: 0.25, intercept: 1}.JSON(),
			wantOut:  "rejected: the error rule failed: ",
		},
		{
			name: "missing om_rounds column",
			setup: func(t *testing.T, fix trainTestFixture) {
				rewriteAutoAdoptLandings(t, fix, []string{"om_rounds"}, nil)
			},
			proposed: adoptTestWeights{boxes: 0.25, intercept: 1}.JSON(),
			wantErr:  "no om_rounds column",
		},
		{
			name: "missing polecat_attempts column",
			setup: func(t *testing.T, fix trainTestFixture) {
				rewriteAutoAdoptLandings(t, fix, []string{"polecat_attempts"}, nil)
			},
			proposed: adoptTestWeights{boxes: 0.25, intercept: 1}.JSON(),
			wantErr:  "no polecat_attempts column",
		},
		{
			name: "om_rounds cell that is not an integer",
			setup: func(t *testing.T, fix trainTestFixture) {
				rewriteAutoAdoptLandings(t, fix, nil, func(i int, row Row) {
					if i == 3 {
						row["om_rounds"] = "x"
					}
				})
			},
			proposed: adoptTestWeights{boxes: 0.25, intercept: 1}.JSON(),
			wantErr:  `bead gt-t003: om_rounds "x" is not an integer`,
		},
		{
			name: "polecat_attempts cell that is not an integer",
			setup: func(t *testing.T, fix trainTestFixture) {
				rewriteAutoAdoptLandings(t, fix, nil, func(i int, row Row) {
					if i == 3 {
						row["polecat_attempts"] = "2.5"
					}
				})
			},
			proposed: adoptTestWeights{boxes: 0.25, intercept: 1}.JSON(),
			wantErr:  `bead gt-t003: polecat_attempts "2.5" is not an integer`,
		},
		{
			// A landing with no om record has an empty cell, which is not an integer, so
			// it refuses rather than being counted as not first round.
			name: "empty om_rounds cell",
			setup: func(t *testing.T, fix trainTestFixture) {
				rewriteAutoAdoptLandings(t, fix, nil, func(i int, row Row) {
					if i == 3 {
						row["om_rounds"] = ""
					}
				})
			},
			proposed: adoptTestWeights{boxes: 0.25, intercept: 1}.JSON(),
			wantErr:  `bead gt-t003: om_rounds "" is not an integer`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			if tc.setup != nil {
				tc.setup(t, fix)
			}
			previous := tc.current
			if previous == "" {
				previous = adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
			}
			writeAt(t, adoptRuntimePath(fix.town), previous)
			writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), tc.proposed)

			var out strings.Builder
			c := fix.adoptCommands(&out, weeklyBase(t), trainTestEmbedder{}, false)
			err := autoAdoptDecide(t, c, fix)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to name %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("decide: %v", err)
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("out = %q, want it to name %q", out.String(), tc.wantOut)
			}
			requireNoRuntimeWrite(t, fix.town, previous)
		})
	}
}

// TestStoryPointsAutoAdoptRefusesAnUnreadableHoldout: when the bead store cannot be
// read, the holdout cannot be built, so the step returns the error and adopts
// nothing.
func TestStoryPointsAutoAdoptRefusesAnUnreadableHoldout(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(fix.town), previous)
	writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), adoptTestWeights{boxes: 0.25, intercept: 1}.JSON())

	var out strings.Builder
	c := fix.adoptCommands(&out, weeklyBase(t), trainTestEmbedder{}, false)
	c.Env.Beads = func(context.Context, string) BeadStore {
		return trainTestStore{err: errors.New("bead store is down")}
	}
	err := autoAdoptDecide(t, c, fix)
	if err == nil || !strings.Contains(err.Error(), "bead store is down") {
		t.Fatalf("err = %v, want the store's error", err)
	}
	requireNoRuntimeWrite(t, fix.town, previous)
}

// TestStoryPointsAutoAdoptWeeklyNamesTheMissingColumn: the weekly run with the key
// on and no om_rounds column still retrains, and its line and error say why the
// adoption was refused. The week is spent, since the next hour meets the same table.
func TestStoryPointsAutoAdoptWeeklyNamesTheMissingColumn(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	rewriteAutoAdoptLandings(t, fix, []string{"om_rounds"}, nil)
	writeWeeklyLastRun(t, fix.town, weeklyNow.Add(-8*24*time.Hour))
	var out strings.Builder
	c := weeklyCommands(t, fix, &out, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	c.StoryPointsAutoAdopt = true
	err := c.RunStoryPointsWeekly(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no om_rounds column") {
		t.Errorf("err = %v, want the missing column named", err)
	}
	if !strings.Contains(out.String(), "story points weekly: failed: ") || !strings.Contains(out.String(), "om_rounds") {
		t.Errorf("out = %q, want the failure and the column on the line", out.String())
	}
	requireWeeklyProposal(t, fix.outDir)
	requireNoWeeklyWeights(t, fix.town)
	if stamp := weeklyLastRun(t, fix.town); stamp != weeklyNow.Format(time.RFC3339) {
		t.Errorf("last run = %q, want the failed week spent at %s", stamp, weeklyNow.Format(time.RFC3339))
	}
}

// TestStoryPointsAutoAdoptOffPathIgnoresFirstRoundCells: with the key off the
// first-round cells are never read, so a malformed one fails neither the retrain
// nor the weekly run, and the proposal is the one a clean table gives.
func TestStoryPointsAutoAdoptOffPathIgnoresFirstRoundCells(t *testing.T) {
	t.Parallel()
	clean := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	badBeads := trainTestBeads(90, 1, true)
	badBeads[3].rounds = "x"
	badBeads[4].attempts = "x"
	bad := newTrainTestFixture(t, badBeads)

	var cleanOut strings.Builder
	cleanCommands := clean.commands(t, &cleanOut, trainTestEmbedder{}, clean.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, cleanCommands); err != nil {
		t.Fatalf("storypoints-train on a clean table: %v", err)
	}
	want := readAt(t, filepath.Join(clean.outDir, storyPointsProposedName))

	var badOut strings.Builder
	badCommands := bad.commands(t, &badOut, trainTestEmbedder{}, bad.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, badCommands); err != nil {
		t.Fatalf("storypoints-train with a malformed om_rounds and polecat_attempts cell: %v", err)
	}
	if got := readAt(t, filepath.Join(bad.outDir, storyPointsProposedName)); got != want {
		t.Errorf("proposal with malformed cells differs from the clean one")
	}

	writeWeeklyLastRun(t, bad.town, weeklyNow.Add(-8*24*time.Hour))
	var weeklyOut strings.Builder
	c := weeklyCommands(t, bad, &weeklyOut, weeklyNow, weeklyBase(t), trainTestEmbedder{})
	if err := c.RunStoryPointsWeekly(context.Background()); err != nil {
		t.Fatalf("RunStoryPointsWeekly with the key off: %v", err)
	}
	if !strings.HasPrefix(weeklyOut.String(), "story points weekly: not adopted: ") {
		t.Errorf("out = %q, want the key-off line", weeklyOut.String())
	}
}

// TestStoryPointsAutoAdoptMarksFirstRound: with the first-round read on, a landing
// is first round only when its om_rounds and polecat_attempts cells are both one.
// With it off, no sample is marked.
func TestStoryPointsAutoAdoptMarksFirstRound(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(90, 1, true)
	beads[0].rounds = "2"
	beads[1].attempts = "2"
	fix := newTrainTestFixture(t, beads)
	data := readAt(t, filepath.Join(fix.outDir, landingsName))
	_, rows, err := ReadLandings(bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	env := Env{Town: fix.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return fix.store() }}

	on, _, err := trainingSamplesWith(context.Background(), env, fix.town, rows, storypoints.Default(), trainTestEmbedder{}, true)
	if err != nil {
		t.Fatalf("trainingSamplesWith: %v", err)
	}
	for i, want := range map[int]bool{0: false, 1: false, 2: true, 3: true} {
		if got := sampleLanded(t, on, i).FirstRound; got != want {
			t.Errorf("bead %d FirstRound = %v, want %v", i, got, want)
		}
	}

	off, _, err := trainingSamples(context.Background(), env, fix.town, rows, storypoints.Default(), trainTestEmbedder{})
	if err != nil {
		t.Fatalf("trainingSamples: %v", err)
	}
	for _, s := range off {
		if s.FirstRound {
			t.Errorf("sample landed %s marked first round with the read off", s.LandedAt)
		}
	}
}
