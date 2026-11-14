package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// trainTestDim is the embedding dimension of the fixture's weights and embedder.
const trainTestDim = 3

// trainTestBaseJSON is a checked-in-shaped weights file of trainTestDim: one minute
// for every bead on the shape model, and an embedding ridge of zeros.
const trainTestBaseJSON = `{"version":1,
	"shape":{"features":["boxes","paths","pkgs","dlen","ncons","nlab","podeng","tests","nbt","rig_gt","rig_dv","is_haiku"],
	"mean":[0,0,0,0,0,0,0,0,0,0,0,0],"scale":[1,1,1,1,1,1,1,1,1,1,1,1],"coef":[0,0,0,0,0,0,0,0,0,0,0,0],"intercept":0,"alpha":1},
	"fits_twenty_minutes_to_minutes_knots":[[0,1],[1,100]],
	"embedding":{"model":"test-embed","dim":3,"input":"test input","alpha":1,"intercept":0,"coef":[0,0,0]}}`

// trainTestBase is the fixture's checked-in weights.
func trainTestBase(t *testing.T) storypoints.Weights {
	t.Helper()
	w, err := storypoints.LoadWeights(strings.NewReader(trainTestBaseJSON))
	if err != nil {
		t.Fatalf("LoadWeights(test base): %v", err)
	}
	return w
}

// trainTestEmbedder embeds a text as a vector of its length, so a bead with more
// acceptance boxes embeds differently. fail makes every call an error.
type trainTestEmbedder struct {
	fail bool
}

func (e trainTestEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if e.fail {
		return nil, errors.New("embedder unreachable")
	}
	v := make([]float32, trainTestDim)
	for i := range v {
		v[i] = float32(len(text)) / 1000 * float32(i+1)
	}
	return v, nil
}

// trainTestStore is a bead store that holds the fixture's beads.
type trainTestStore struct {
	issues map[string]*beads.Issue
	// err, when set, is the answer of every read: a store that is down.
	err error
}

func (s trainTestStore) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, nil }

func (s trainTestStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) { return nil, nil }

func (s trainTestStore) Blocked() ([]*beads.Issue, error) { return nil, nil }

func (s trainTestStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := s.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

// trainTestBead is one landed bead of the fixture: its acceptance boxes drive the
// minutes it took when the fixture has a signal.
type trainTestBead struct {
	id         string
	seat       string
	boxes      int
	minutes    float64
	bound      string // the polecat_lower_bound cell: "0" measured, "1" a lower bound
	noText     bool   // the bead has no Constraints or Acceptance section
	missing    bool   // the bead is not in the store
	badTime    bool   // the landing time does not parse
	notPolecat bool   // the landing did not come through a polecat seat
	// sameTimeAsPrev gives the bead the landing time of the bead before it.
	sameTimeAsPrev bool
}

// badTimeBeads is a measured fixture whose first landing time does not parse.
func badTimeBeads() []trainTestBead {
	beads := trainTestBeads(90, 1, true)
	beads[0].badTime = true
	return beads
}

// trainTestBeads draws n measured beads in landing order. With signal, the minutes
// grow with the acceptance boxes; without it they are unrelated to the bead.
func trainTestBeads(n int, seed uint64, signal bool) []trainTestBead {
	rng := rand.New(rand.NewPCG(seed, seed))
	out := make([]trainTestBead, n)
	for i := range out {
		boxes := 1 + rng.IntN(10)
		noise := 0.2 * (rng.Float64() - 0.5)
		minutes := math.Exp(2 + 2*rng.Float64())
		if signal {
			minutes = math.Exp(0.25*float64(boxes) + noise + 1)
		}
		seat := "haiku"
		if i%3 == 0 {
			seat = "flash"
		}
		out[i] = trainTestBead{id: fmt.Sprintf("gt-t%03d", i), seat: seat, boxes: boxes, minutes: minutes, bound: "0"}
	}
	return out
}

// trainTestIssue is the bead the fixture's store holds for one landed bead.
func trainTestIssue(b trainTestBead) *beads.Issue {
	var desc strings.Builder
	if !b.noText {
		desc.WriteString("## Constraints\n- keep the change small\n\n## Acceptance\n")
		for i := 0; i < b.boxes; i++ {
			fmt.Fprintf(&desc, "- [ ] check %d\n", i)
		}
	}
	return &beads.Issue{
		ID:          b.id,
		Title:       "fixture " + b.id,
		Description: desc.String(),
		Status:      "closed",
		Type:        "task",
		Priority:    2,
	}
}

// trainTestFixture is the town a retrain reads: the day's landings table in the
// extract's output directory, the beads, and the seams the step reads through.
type trainTestFixture struct {
	town   string
	outDir string
	beads  []trainTestBead
}

// newTrainTestFixture writes the landings table for beads under a temp town and
// returns the fixture. The landings rows carry the landing times the retrain orders
// by, one hour apart.
func newTrainTestFixture(t *testing.T, beadsIn []trainTestBead) trainTestFixture {
	t.Helper()
	town := t.TempDir()
	outDir := filepath.Join(town, ".runtime", "metrics", commandToday)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]Row, 0, len(beadsIn))
	var prev time.Time
	for i, b := range beadsIn {
		landed := start.Add(time.Duration(i) * time.Hour)
		if b.sameTimeAsPrev && i > 0 {
			landed = prev
		}
		prev = landed
		landedText := landed.Format(time.RFC3339)
		if b.badTime {
			landedText = "last tuesday"
		}
		polecatWork := "1"
		if b.notPolecat {
			polecatWork = "0"
		}
		rows = append(rows, Row{
			"bead":                b.id,
			"rig":                 "gastown",
			"landed_at":           landedText,
			"seat_class":          b.seat,
			"polecat_secs":        fmt.Sprintf("%g", b.minutes*60),
			"polecat_lower_bound": b.bound,
			"polecat_work":        polecatWork,
		})
	}
	var csvBuf strings.Builder
	if err := WriteLandings(&csvBuf, landingColumns, rows); err != nil {
		t.Fatalf("WriteLandings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, landingsName), []byte(csvBuf.String()), 0o644); err != nil {
		t.Fatalf("write landings: %v", err)
	}
	return trainTestFixture{town: town, outDir: outDir, beads: beadsIn}
}

// commands is the step wired to the fixture, with the embedder and the bead store
// the case names.
func (f trainTestFixture) commands(t *testing.T, out *strings.Builder, emb storypoints.Embedder, store trainTestStore) Commands {
	t.Helper()
	base := trainTestBase(t)
	return Commands{
		Today: commandToday,
		Wire: func(context.Context) (RealTown, error) {
			return RealTown{Town: f.town, Extract: ExtractEnv{OutDir: f.outDir}}, nil
		},
		Env: Env{
			Town:     f.town,
			ReadFile: os.ReadFile,
			Beads:    func(context.Context, string) BeadStore { return store },
		},
		Out:             out,
		StoryPointsBase: &base,
		Embedder:        emb,
	}
}

// store returns the bead store holding every fixture bead the case did not hide.
func (f trainTestFixture) store() trainTestStore {
	issues := map[string]*beads.Issue{}
	for _, b := range f.beads {
		if !b.missing {
			issues[b.id] = trainTestIssue(b)
		}
	}
	return trainTestStore{issues: issues}
}

// TestStoryPointsTrainWritesProposedFileThatLoads: the step writes a proposed weights
// file that LoadWeights accepts into the day's output directory, names the report
// beside it, and writes nothing else under the town. The fixture's minutes follow the
// acceptance boxes, so the holdout clears the bar and the file is not marked rejected.
func TestStoryPointsTrainWritesProposedFileThatLoads(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}

	proposed := filepath.Join(fix.outDir, storyPointsProposedName)
	f, err := os.Open(proposed)
	if err != nil {
		t.Fatalf("proposed file: %v", err)
	}
	defer f.Close()
	if _, err := storypoints.LoadWeights(f); err != nil {
		t.Errorf("LoadWeights(proposed): %v", err)
	}
	if !strings.Contains(out.String(), "clears the bar") {
		t.Errorf("report does not say the proposed weights clear the bar:\n%s", out.String())
	}

	err = filepath.WalkDir(fix.town, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Base(path) == "weights.json" {
			t.Errorf("step wrote %s", path)
		}
		if !strings.HasPrefix(path, fix.outDir+string(filepath.Separator)) && filepath.Base(path) != landingsName {
			t.Errorf("step wrote %s outside the output directory", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk town: %v", err)
	}
}

// TestStoryPointsTrainRejectsAHoldoutBelowTheBar: when the minutes carry no signal the
// holdout correlation is below 0.4, so the proposed file is written with the rejected
// suffix and the report says so.
func TestStoryPointsTrainRejectsAHoldoutBelowTheBar(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 2, false))
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unsuffixed proposed file exists (stat err %v), want only the rejected one", err)
	}
	rejected := filepath.Join(fix.outDir, storyPointsProposedName+storyPointsRejectedSuffix)
	if _, err := os.Stat(rejected); err != nil {
		t.Fatalf("rejected file: %v", err)
	}
	// The holdout is measured and below the bar; a report that only failed to measure
	// it would pass the file check for the wrong reason.
	report := out.String()
	if strings.Contains(report, "proposed weights: not measurable") {
		t.Errorf("holdout was not measured, so the rejection is not the bar:\n%s", report)
	}
	if !strings.Contains(report, "(bar 0.40)") || !strings.Contains(report, "verdict: below the bar") {
		t.Errorf("report does not give the holdout correlation and the verdict:\n%s", report)
	}
}

// TestStoryPointsTrainRefusals: each case the step cannot fit refuses with an error,
// and writes no proposed file.
func TestStoryPointsTrainRefusals(t *testing.T) {
	t.Parallel()
	// blockAt makes a directory where a file would be written, so the write fails.
	blockAt := func(name string) func(t *testing.T, outDir string) {
		return func(t *testing.T, outDir string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(outDir, name), 0o755); err != nil {
				t.Fatalf("block %s: %v", name, err)
			}
		}
	}
	// dropAt rewrites the fixture's landings table without one column, the way an
	// extract from before that column wrote it.
	dropAt := func(column string) func(t *testing.T, outDir string) {
		return func(t *testing.T, outDir string) {
			t.Helper()
			path := filepath.Join(outDir, landingsName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read landings: %v", err)
			}
			header, rows, err := ReadLandings(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("ReadLandings: %v", err)
			}
			i := slices.Index(header, column)
			if i < 0 {
				t.Fatalf("the fixture's header has no %s column", column)
			}
			var buf strings.Builder
			if err := WriteLandings(&buf, slices.Delete(header, i, i+1), rows); err != nil {
				t.Fatalf("WriteLandings: %v", err)
			}
			if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
				t.Fatalf("write landings: %v", err)
			}
		}
	}
	cases := []struct {
		name     string
		beads    []trainTestBead
		emb      storypoints.Embedder
		setup    func(t *testing.T, outDir string)
		storeErr error
		// wrote is set when the step fails after it has written the proposed file: a
		// report that cannot be written comes after the proposed file.
		wrote   bool
		wantErr string
	}{
		{name: "no landings table", beads: trainTestBeads(90, 1, true), wantErr: CommandExtract,
			setup: func(t *testing.T, outDir string) {
				if err := os.Remove(filepath.Join(outDir, landingsName)); err != nil {
					t.Fatalf("remove landings: %v", err)
				}
			}},
		{name: "unreadable landings table", beads: trainTestBeads(90, 1, true), wantErr: landingsName,
			setup: func(t *testing.T, outDir string) {
				if err := os.WriteFile(filepath.Join(outDir, landingsName), []byte("bead,rig\n\"open\n"), 0o644); err != nil {
					t.Fatalf("corrupt landings: %v", err)
				}
			}},
		{name: "landings table from before the polecat_work column", beads: trainTestBeads(90, 1, true),
			wantErr: "polecat_work", setup: dropAt("polecat_work")},
		{name: "landing time that does not parse", beads: badTimeBeads(), wantErr: "invalid ISO timestamp"},
		{name: "too few measured beads", beads: trainTestBeads(30, 3, true), wantErr: "samples"},
		{name: "embedder fails", beads: trainTestBeads(90, 4, true), emb: trainTestEmbedder{fail: true}, wantErr: "embed bead"},
		{name: "bead store fails", beads: trainTestBeads(90, 1, true), storeErr: errors.New("bd unreachable"), wantErr: "read beads"},
		{name: "proposed file cannot be written", beads: trainTestBeads(90, 1, true), wantErr: storyPointsProposedName,
			setup: blockAt(storyPointsProposedName)},
		{name: "report cannot be written", beads: trainTestBeads(90, 1, true), wrote: true, wantErr: storyPointsTrainReportName,
			setup: blockAt(storyPointsTrainReportName)},
		// The output path is a file, so the names under it cannot be looked up: the run
		// refuses before it reads anything.
		{name: "output directory is a file", beads: trainTestBeads(90, 1, true), wantErr: storyPointsProposedName,
			setup: func(t *testing.T, outDir string) {
				if err := os.Rename(outDir, outDir+".moved"); err != nil {
					t.Fatalf("move output directory: %v", err)
				}
				if err := os.WriteFile(outDir, []byte("not a directory\n"), 0o644); err != nil {
					t.Fatalf("write output path as a file: %v", err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, tc.beads)
			if tc.setup != nil {
				tc.setup(t, fix.outDir)
			}
			emb := tc.emb
			if emb == nil {
				emb = trainTestEmbedder{}
			}
			store := fix.store()
			store.err = tc.storeErr
			var out strings.Builder
			c := fix.commands(t, &out, emb, store)
			err := Run(context.Background(), CommandStoryPointsTrain, c)
			if err == nil {
				t.Fatalf("storypoints-train returned no error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to name %q", err, tc.wantErr)
			}
			info, statErr := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName))
			if statErr == nil && !info.IsDir() && !tc.wrote {
				t.Errorf("a refused run wrote a proposed file")
			}
		})
	}
}

// TestStoryPointsTrainSkipsAndCountsUnreadBeads: a landing with no time, a landing
// that is not polecat work, a bead not in the store and a bead with no section text
// are left out of the fit and counted in the report; the rest of the table is fitted.
func TestStoryPointsTrainSkipsAndCountsUnreadBeads(t *testing.T) {
	t.Parallel()
	bead := trainTestBeads(90, 5, true)
	bead[0].minutes = 0
	bead[1].missing = true
	bead[2].noText = true
	bead[3].notPolecat = true
	fix := newTrainTestFixture(t, bead)
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	want := "not read: not polecat work 1, seat class 0, unmeasured 1, log line without an embedding 0, log line embedding of another size 0, bead not in store 1, no Constraints or Acceptance 1, rig without a directory 0"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report = %q, want the line %q", out.String(), want)
	}
	// The 86 beads read are split: the earlier 69 are the fit and the latest 17 the holdout.
	if !strings.Contains(out.String(), "n=69)") {
		t.Errorf("report does not count 69 fitted beads:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "holdout: latest 17 beads") {
		t.Errorf("report does not count 17 holdout beads:\n%s", out.String())
	}
}

// TestStoryPointsTrainFitsOnlyPolecatLandings: the retrain reads a landing that came
// through a polecat seat and leaves an overseer, bird or crew landing out, whatever
// time the table records for it, and the report counts the rows it skipped.
func TestStoryPointsTrainFitsOnlyPolecatLandings(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(110, 9, true)
	for i := 0; i < 30; i++ {
		beads[i].notPolecat = true
	}
	fix := newTrainTestFixture(t, beads)
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	want := "not read: not polecat work 30, seat class 0, unmeasured 0, log line without an embedding 0, log line embedding of another size 0, bead not in store 0, no Constraints or Acceptance 0, rig without a directory 0"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report = %q, want the line %q", out.String(), want)
	}
	// The 80 polecat landings are split: the earlier 64 fit and the latest 16 hold out.
	if !strings.Contains(out.String(), "n=64)") {
		t.Errorf("report does not count the 64 fitted beads:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "holdout: latest 16 beads") {
		t.Errorf("report does not count the 16 holdout beads:\n%s", out.String())
	}
}

// TestStoryPointsTrainKeepsALowerBoundAtItsFloor: a lower-bound time stays in the fit
// at its floor value rather than being skipped as unmeasured, so the long beads the
// bound marks are not dropped.
func TestStoryPointsTrainKeepsALowerBoundAtItsFloor(t *testing.T) {
	t.Parallel()
	const floorMinutes = 7
	beads := trainTestBeads(3, 11, true)
	beads[0].bound = "1"
	beads[0].minutes = floorMinutes
	fix := newTrainTestFixture(t, beads)
	data, err := os.ReadFile(filepath.Join(fix.outDir, landingsName))
	if err != nil {
		t.Fatalf("read landings: %v", err)
	}
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	env := Env{Town: fix.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return fix.store() }}
	samples, counts, err := trainingSamples(context.Background(), env, fix.town, rows, storypoints.Default(), trainTestEmbedder{})
	if err != nil {
		t.Fatalf("trainingSamples: %v", err)
	}
	if counts.skips.unmeasured != 0 || counts.skips.notPolecat != 0 {
		t.Errorf("counts = %+v, want the lower-bound row read", counts)
	}
	// The fixture lands the lower-bound bead first, an hour before the others.
	landed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var fitted *storypoints.Sample
	for i := range samples {
		if samples[i].LandedAt.Equal(landed) {
			fitted = &samples[i]
		}
	}
	if fitted == nil {
		t.Fatalf("no sample landed at %s; samples = %d", landed, len(samples))
	}
	if fitted.Minutes != floorMinutes {
		t.Errorf("lower-bound bead fitted at %v minutes, want its floor %d", fitted.Minutes, floorMinutes)
	}
}

// TestStoryPointsTrainLeavesTheShadowReportUnfiltered: the shadow report counts every
// estimated landing with a measured time, including one the retrain skips as not
// polecat work, so the retrain's filter never shrinks the report.
func TestStoryPointsTrainLeavesTheShadowReportUnfiltered(t *testing.T) {
	t.Parallel()
	rows := measuredRows(59)
	crew := spLandingRow(99, "8", "3", "haiku", "all")
	crew["polecat_work"] = "0"
	rows = append(rows, crew)
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, rows); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	want := "story points shadow report: 60 estimated landings with a measured time\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report does not count the non-polecat landing, want %q:\n%s", want, out.String())
	}
}

// TestStoryPointsTrainRemovesStaleProposedFiles: a run removes the proposed file an
// earlier run left under either name, so the day's directory holds only the one file
// this run writes.
func TestStoryPointsTrainRemovesStaleProposedFiles(t *testing.T) {
	t.Parallel()
	proposed := storyPointsProposedName
	rejected := storyPointsProposedName + storyPointsRejectedSuffix
	cases := []struct {
		name  string
		beads []trainTestBead
		stale string
		want  string
	}{
		// Seed 2 with no signal is rejected (see TestStoryPointsTrainRejectsAHoldoutBelowTheBar).
		{"a rejecting run clears a passing file", trainTestBeads(90, 2, false), proposed, rejected},
		// Seed 1 with signal clears the bar (see TestStoryPointsTrainWritesProposedFileThatLoads).
		{"a passing run clears a rejected file", trainTestBeads(90, 1, true), rejected, proposed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, tc.beads)
			if err := os.WriteFile(filepath.Join(fix.outDir, tc.stale), []byte("stale\n"), 0o644); err != nil {
				t.Fatalf("write stale file: %v", err)
			}
			var out strings.Builder
			c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
			if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
				t.Fatalf("storypoints-train: %v", err)
			}
			if _, err := os.Stat(filepath.Join(fix.outDir, tc.want)); err != nil {
				t.Errorf("this run's file %s: %v", tc.want, err)
			}
			if _, err := os.Stat(filepath.Join(fix.outDir, tc.stale)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("stale %s survived the run (stat err %v)", tc.stale, err)
			}
		})
	}
}

// TestRemoveStoryPointsOutputsReportsARemovalFailure: a stale file the step cannot remove
// stops the run with an error that names the file, so the file is never read as the
// verdict of this run.
func TestRemoveStoryPointsOutputsReportsARemovalFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storyPointsProposedName), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	boom := errors.New("read-only directory")
	err := removeStoryPointsOutputs(dir, func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the remove error", err)
	}
	if !strings.Contains(err.Error(), storyPointsProposedName) {
		t.Errorf("err = %q, want it to name %s", err, storyPointsProposedName)
	}
}

// TestStoryPointsTrainRejectsAHoldoutTooShortToRead: when the latest beads share a
// landing time the holdout boundary moves past them, leaving eight holdout beads. That
// is fewer than storyPointsMinHoldout, so the correlation is not measurable and the
// proposed file is rejected whatever the fit.
func TestStoryPointsTrainRejectsAHoldoutTooShortToRead(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(60, 6, true)
	for i := 47; i <= 51; i++ {
		beads[i].sameTimeAsPrev = true
	}
	fix := newTrainTestFixture(t, beads)
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unsuffixed proposed file exists (stat err %v), want only the rejected one", err)
	}
	if _, err := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName+storyPointsRejectedSuffix)); err != nil {
		t.Fatalf("rejected file: %v", err)
	}
	want := fmt.Sprintf("holdout rank correlation of the proposed weights: not measurable (8 holdout beads, want at least %d)", storyPointsMinHoldout)
	if !strings.Contains(out.String(), want) {
		t.Errorf("report does not say the holdout is too short, want %q:\n%s", want, out.String())
	}
}

// TestStoryPointsTrainUnmeasurableHoldout: a holdout that no rank correlation can be read
// from decides the file name by the proposed model alone. An unmeasurable proposed model
// is rejected. An unmeasurable checked-in model is reported and does not reject a good
// proposal.
func TestStoryPointsTrainUnmeasurableHoldout(t *testing.T) {
	t.Parallel()
	// holdout builds n holdout beads whose measured minutes rise with their position, so
	// the predictions alone decide whether a correlation is measurable.
	holdout := func(n int, proposed, checked func(i int) float64) []storypoints.Holdout {
		hold := make([]storypoints.Holdout, n)
		for i := range hold {
			hold[i] = storypoints.Holdout{Actual: float64(i + 1), Proposed: proposed(i), Checked: checked(i)}
		}
		return hold
	}
	constant := func(int) float64 { return 5 }
	rising := func(i int) float64 { return float64(i + 1) }
	rejected := storyPointsProposedName + storyPointsRejectedSuffix
	cases := []struct {
		name     string
		holdout  []storypoints.Holdout
		wantName string
		wantLine string
	}{
		{"constant proposed predictions", holdout(12, constant, rising), rejected,
			"holdout rank correlation of the proposed weights: not measurable"},
		{"too few holdout beads", holdout(storyPointsMinHoldout-1, rising, rising), rejected,
			"holdout rank correlation of the proposed weights: not measurable"},
		{"constant checked-in predictions, good proposal", holdout(12, rising, constant), storyPointsProposedName,
			"checked-in: not measurable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			score := holdoutRanks(tc.holdout)
			name := score.fileName()
			if name != tc.wantName {
				t.Errorf("file name = %q, want %q", name, tc.wantName)
			}
			report := storyPointsTrainReport(storypoints.Model{Holdout: tc.holdout, TrainedOn: "fixture"}, trainCounts{}, name, score)
			if !strings.Contains(report, tc.wantLine) {
				t.Errorf("report does not say %q:\n%s", tc.wantLine, report)
			}
		})
	}
}

// TestStoryPointsTrainReportsTheScoredFile: the proposed file the step writes is the model
// the report scores. LoadWeights reads it back, the holdout beads are scored with it, and
// the rank correlation is the one the report prints. The first line says the file is the
// earlier-80% fit, and the knots line says they are the checked-in model's.
func TestStoryPointsTrainReportsTheScoredFile(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	store := fix.store()
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, store)
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	report := out.String()
	firstLine, _, _ := strings.Cut(report, "\n")
	if !strings.Contains(firstLine, "the proposed weights are the fit on the earlier 80%") {
		t.Errorf("first line = %q, want it to say the proposed weights are the earlier 80%% fit", firstLine)
	}
	const knots = "fits-to-minutes knots are the checked-in model's, not refit: recalibrate before adopting"
	if !strings.Contains(report, knots) {
		t.Errorf("report does not carry the knots line %q:\n%s", knots, report)
	}

	f, err := os.Open(filepath.Join(fix.outDir, storyPointsProposedName))
	if err != nil {
		t.Fatalf("proposed file: %v", err)
	}
	defer f.Close()
	read, err := storypoints.LoadWeights(f)
	if err != nil {
		t.Fatalf("LoadWeights(proposed): %v", err)
	}

	// The step's own samples, in the order it fitted them: the latest 18 of the 90 beads
	// are the holdout, and the earlier 72 are the fit.
	data, err := os.ReadFile(filepath.Join(fix.outDir, landingsName))
	if err != nil {
		t.Fatalf("read landings: %v", err)
	}
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	env := Env{Town: fix.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return store }}
	samples, _, err := trainingSamples(context.Background(), env, fix.town, rows, storypoints.Default(), trainTestEmbedder{})
	if err != nil {
		t.Fatalf("trainingSamples: %v", err)
	}
	var actual, predicted []float64
	for _, s := range samples[72:] {
		shape, err := read.ShapeMinutes(s.Shape)
		if err != nil {
			t.Fatalf("ShapeMinutes: %v", err)
		}
		embed, err := read.EmbeddingMinutes(s.Vector)
		if err != nil {
			t.Fatalf("EmbeddingMinutes: %v", err)
		}
		actual = append(actual, s.Minutes)
		predicted = append(predicted, math.Sqrt(shape*embed))
	}
	rho, err := Spearman(actual, predicted)
	if err != nil {
		t.Fatalf("Spearman over the holdout: %v", err)
	}
	want := fmt.Sprintf("holdout rank correlation of the proposed weights: %.3f", rho)
	if !strings.Contains(report, want) {
		t.Errorf("report does not print the correlation of the written file, want %q:\n%s", want, report)
	}
}

// TestStoryPointsEmbedderReadsItsURLFromTheEnvironment: the default embedder sends to the
// URL the environment names, or to the local default when the name is unset or empty. It
// takes the model and dimension from the checked-in weights and the bound from the step.
// No request is sent.
func TestStoryPointsEmbedderReadsItsURLFromTheEnvironment(t *testing.T) {
	t.Parallel()
	base := trainTestBase(t)
	cases := []struct {
		name    string
		setting string
		want    string
	}{
		{"set", "http://embed.example:9999", "http://embed.example:9999"},
		{"empty", "", storyPointsEmbedDefaultURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(name string) string {
				if name != storyPointsEmbedURLEnv {
					t.Errorf("looked up %q, want %q", name, storyPointsEmbedURLEnv)
				}
				return tc.setting
			}
			e := storyPointsEmbedder(base, getenv)
			if e.URL != tc.want {
				t.Errorf("URL = %q, want %q", e.URL, tc.want)
			}
			if e.Timeout != storyPointsEmbedTimeout {
				t.Errorf("Timeout = %v, want %v", e.Timeout, storyPointsEmbedTimeout)
			}
			if e.Model != "test-embed" || e.Dim != trainTestDim {
				t.Errorf("Model, Dim = %q, %d; want the checked-in model and dimension", e.Model, e.Dim)
			}
			if e.HTTP == nil {
				t.Errorf("HTTP client is nil, so the embedder would refuse every request")
			}
		})
	}
}

// logRecord is one estimate log line for a fixture bead: the features the
// estimator read, for the text its hash names.
func logRecord(bead, hash string, shape estlog.Shape, vec []float32) estlog.Record {
	return estlog.Record{
		BeadID:          bead,
		DescriptionHash: hash,
		Shape:           shape,
		Embedding:       vec,
		Time:            time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

// appendLog appends the records to the fixture town's estimate log through the
// production writer, so the step reads the file the estimator writes.
func (f trainTestFixture) appendLog(t *testing.T, records ...estlog.Record) {
	t.Helper()
	for _, r := range records {
		if err := estlog.Append(f.town, r); err != nil {
			t.Fatalf("append estimate log: %v", err)
		}
	}
}

// fixtureLanded is the landing time newTrainTestFixture gives its bead i.
func fixtureLanded(i int) time.Time {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
}

// sampleLanded returns the sample the fixture's bead i produced.
func sampleLanded(t *testing.T, samples []storypoints.Sample, i int) storypoints.Sample {
	t.Helper()
	for _, s := range samples {
		if s.LandedAt.Equal(fixtureLanded(i)) {
			return s
		}
	}
	t.Fatalf("no sample landed at %s; samples = %d", fixtureLanded(i), len(samples))
	return storypoints.Sample{}
}

// TestStoryPointsTrainFitsTheLoggedFeatures: every bead's current text carries
// one acceptance box, so a fit on it cannot rank the beads, while each bead's
// log line carries the box count its minutes followed. The proposed file clears
// the bar only when the step reads the log lines, and the report counts the
// beads it read them for.
func TestStoryPointsTrainFitsTheLoggedFeatures(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(90, 21, true)
	var records []estlog.Record
	for i := range beads {
		loggedBoxes := 1 + i%10
		beads[i].boxes = 1
		beads[i].minutes = math.Exp(0.25*float64(loggedBoxes) + 1)
		records = append(records, logRecord(beads[i].id, "an-earlier-text", estlog.Shape{Boxes: float64(loggedBoxes)}, make([]float32, trainTestDim)))
	}
	fix := newTrainTestFixture(t, beads)
	fix.appendLog(t, records...)
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); err != nil {
		t.Fatalf("proposed file: %v; the fit did not read the log lines:\n%s", err, out.String())
	}
	want := "read: from the estimate log 90, from the current text 0"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report does not say %q:\n%s", want, out.String())
	}
}

// TestStoryPointsTrainReadsTheBeadsLastLogLine: a landed bead is fitted on the
// last estimate-log line that names it, whatever its text says now. The log
// keeps one line per bead and description hash, so that line is the latest
// estimate of the bead's text: a bead whose text differs from the logged hash
// still fits on what the estimator saw, and a line belongs to the bead it names.
func TestStoryPointsTrainReadsTheBeadsLastLogLine(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(2, 22, true)
	currentHash := estimate.SectionHash(trainTestIssue(beads[0]))
	matching := logRecord(beads[0].id, currentHash, estlog.Shape{Boxes: 5}, []float32{0.5, 0.5, 0.5})
	earlier := logRecord(beads[0].id, "an-earlier-text", estlog.Shape{Boxes: 2}, []float32{0.2, 0.2, 0.2})
	later := logRecord(beads[0].id, "a-later-text", estlog.Shape{Boxes: 9}, []float32{0.9, 0.9, 0.9})
	other := logRecord(beads[1].id, "another-beads-text", estlog.Shape{Boxes: 7}, []float32{0.7, 0.7, 0.7})
	// The changed-text case reads a line whose hash is not the bead's text hash,
	// so the fixture must keep those apart.
	if earlier.DescriptionHash == currentHash || later.DescriptionHash == currentHash {
		t.Fatalf("the fixture's logged hashes are the hash of the bead's text now, so the changed-text case is not tested")
	}
	cases := []struct {
		name  string
		lines []estlog.Record
		gone  bool // bead 0 is not in the store
		want  float64
	}{
		{"the latest estimate wins over an earlier one", []estlog.Record{earlier, later}, false, 9},
		{"a line for the text the bead carries now does not outrank a later one", []estlog.Record{matching, later}, false, 9},
		{"each bead reads the line that names it", []estlog.Record{earlier, other}, false, 2},
		{"a bead the store does not hold reads its last line", []estlog.Record{earlier, later}, true, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines := tc.lines
			if !slices.ContainsFunc(lines, func(r estlog.Record) bool { return r.BeadID == beads[1].id }) {
				lines = append(slices.Clone(lines), other)
			}
			// Each case mutates its own beads: the store answers from the slice the
			// fixture holds.
			own := slices.Clone(beads)
			own[0].missing = tc.gone
			fit := newTrainTestFixture(t, own)
			fit.appendLog(t, lines...)
			data, err := os.ReadFile(filepath.Join(fit.outDir, landingsName))
			if err != nil {
				t.Fatalf("read landings: %v", err)
			}
			_, rows, err := ReadLandings(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("ReadLandings: %v", err)
			}
			env := Env{Town: fit.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return fit.store() }}
			samples, counts, err := trainingSamples(context.Background(), env, fit.town, rows, trainTestBase(t), trainTestEmbedder{})
			if err != nil {
				t.Fatalf("trainingSamples: %v", err)
			}
			if got := sampleLanded(t, samples, 0).Shape.Boxes; got != tc.want {
				t.Errorf("bead 0 fitted with %v boxes, want the logged %v", got, tc.want)
			}
			if got := sampleLanded(t, samples, 1).Shape.Boxes; got != 7 {
				t.Errorf("bead 1 fitted with %v boxes, want its own line's 7", got)
			}
			if counts.logged != 2 {
				t.Errorf("counts.logged = %d, want the two beads read from the log", counts.logged)
			}
		})
	}
}

// TestStoryPointsTrainFallsBackToTheCurrentText: a bead with no estimate log
// line is fitted on its text as it is now: the shape features of its estimate
// text and the embedding of its section text.
func TestStoryPointsTrainFallsBackToTheCurrentText(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(1, 23, true)
	fix := newTrainTestFixture(t, beads)
	data, err := os.ReadFile(filepath.Join(fix.outDir, landingsName))
	if err != nil {
		t.Fatalf("read landings: %v", err)
	}
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	store := fix.store()
	env := Env{Town: fix.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return store }}
	base := trainTestBase(t)
	samples, counts, err := trainingSamples(context.Background(), env, fix.town, rows, base, trainTestEmbedder{})
	if err != nil {
		t.Fatalf("trainingSamples: %v", err)
	}
	if counts.currentText != 1 || counts.logged != 0 {
		t.Errorf("counts = %+v, want one bead read from its current text", counts)
	}
	issue := store.issues[beads[0].id]
	seat := storypoints.SeatHaiku
	if beads[0].seat == "flash" {
		seat = storypoints.SeatFlash
	}
	want := storypoints.Features(storypoints.EstimateText(issue), "gastown", seat, base)
	text, err := storypoints.SectionText(issue)
	if err != nil {
		t.Fatalf("SectionText: %v", err)
	}
	wantVec, err := (trainTestEmbedder{}).Embed(context.Background(), text)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	got := sampleLanded(t, samples, 0)
	if got.Shape != want {
		t.Errorf("shape = %+v, want the current text's %+v", got.Shape, want)
	}
	if !slices.Equal(got.Vector, wantVec) {
		t.Errorf("vector = %v, want the current text's %v", got.Vector, wantVec)
	}
}

// TestStoryPointsTrainCountsTheFallbackInTheReport: a bead with no log line is
// fitted on its current text, and the report counts the beads fitted on each
// source.
func TestStoryPointsTrainCountsTheFallbackInTheReport(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(90, 24, true)
	fix := newTrainTestFixture(t, beads)
	var records []estlog.Record
	for i := 0; i < 30; i++ {
		records = append(records, logRecord(beads[i].id, "an-earlier-text", estlog.Shape{Boxes: float64(beads[i].boxes)}, make([]float32, trainTestDim)))
	}
	fix.appendLog(t, records...)
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	want := "read: from the estimate log 30, from the current text 60"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report does not say %q:\n%s", want, out.String())
	}
}

// TestStoryPointsTrainSkipsLogLinesItCannotFit: a log line with no embedding,
// and one whose embedding is not the weights' size, are not fitted and are
// counted; the bead with a line the ridges can read is fitted.
func TestStoryPointsTrainSkipsLogLinesItCannotFit(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(3, 25, true)
	fix := newTrainTestFixture(t, beads)
	fix.appendLog(t,
		logRecord(beads[0].id, "no-embedding", estlog.Shape{Boxes: 2}, nil),
		logRecord(beads[1].id, "another-size", estlog.Shape{Boxes: 3}, make([]float32, trainTestDim+1)),
		logRecord(beads[2].id, "readable", estlog.Shape{Boxes: 4}, make([]float32, trainTestDim)),
	)
	data, err := os.ReadFile(filepath.Join(fix.outDir, landingsName))
	if err != nil {
		t.Fatalf("read landings: %v", err)
	}
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	env := Env{Town: fix.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return fix.store() }}
	samples, counts, err := trainingSamples(context.Background(), env, fix.town, rows, trainTestBase(t), trainTestEmbedder{})
	if err != nil {
		t.Fatalf("trainingSamples: %v", err)
	}
	if len(samples) != 1 || counts.logged != 1 {
		t.Errorf("samples = %d, counts.logged = %d, want only the readable line fitted", len(samples), counts.logged)
	}
	if counts.skips.logNoEmbed != 1 || counts.skips.logEmbedSize != 1 {
		t.Errorf("skips = %+v, want one line of each kind left out", counts.skips)
	}
	report := storyPointsTrainReport(storypoints.Model{TrainedOn: "fixture"}, counts, storyPointsProposedName, holdoutScore{})
	want := "log line without an embedding 1, log line embedding of another size 1"
	if !strings.Contains(report, want) {
		t.Errorf("report does not say %q:\n%s", want, report)
	}
}

// TestStoryPointsTrainRefusesAnUnreadableLog: a log that cannot be read stops
// the step naming the path, rather than reading as no log and fitting every
// bead on text it may have edited.
func TestStoryPointsTrainRefusesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	// The log's path is a directory: opening it succeeds and reading it fails.
	if err := os.MkdirAll(estlog.Path(fix.town), 0o755); err != nil {
		t.Fatalf("make the log path a directory: %v", err)
	}
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	err := Run(context.Background(), CommandStoryPointsTrain, c)
	if err == nil {
		t.Fatalf("storypoints-train returned no error")
	}
	if !strings.Contains(err.Error(), "estimates.jsonl") {
		t.Errorf("err = %v, want it to name the log", err)
	}
	if _, statErr := os.Stat(filepath.Join(fix.outDir, storyPointsProposedName)); statErr == nil {
		t.Errorf("a run that could not read the log wrote a proposed file")
	}
}

// TestStoryPointsTrainCountsTheLogsMalformedLines: a line the log's reader skips
// is counted in the report, so a torn log is visible and not read as a log with
// fewer beads.
func TestStoryPointsTrainCountsTheLogsMalformedLines(t *testing.T) {
	t.Parallel()
	beads := trainTestBeads(90, 1, true)
	fix := newTrainTestFixture(t, beads)
	fix.appendLog(t, logRecord(beads[0].id, "an-earlier-text", estlog.Shape{Boxes: 2}, make([]float32, trainTestDim)))
	f, err := os.OpenFile(estlog.Path(fix.town), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open estimate log: %v", err)
	}
	if _, err := f.WriteString("{\"bead_id\":\n"); err != nil {
		t.Fatalf("append torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close estimate log: %v", err)
	}
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	want := "(the log skipped 1 malformed line(s))"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report does not say %q:\n%s", want, out.String())
	}
}
