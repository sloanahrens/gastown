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
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints"
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
	id      string
	seat    string
	boxes   int
	minutes float64
	bound   string // the polecat_lower_bound cell: "0" measured, "1" a lower bound
	noText  bool   // the bead has no Constraints or Acceptance section
	missing bool   // the bead is not in the store
	badTime bool   // the landing time does not parse
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
		rows = append(rows, Row{
			"bead":                b.id,
			"rig":                 "gastown",
			"landed_at":           landedText,
			"seat_class":          b.seat,
			"polecat_secs":        fmt.Sprintf("%g", b.minutes*60),
			"polecat_lower_bound": b.bound,
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
		{name: "landing time that does not parse", beads: badTimeBeads(), wantErr: "invalid ISO timestamp"},
		{name: "too few measured beads", beads: trainTestBeads(30, 3, true), wantErr: "samples"},
		{name: "embedder fails", beads: trainTestBeads(90, 4, true), emb: trainTestEmbedder{fail: true}, wantErr: "embed bead"},
		{name: "bead store fails", beads: trainTestBeads(90, 1, true), storeErr: errors.New("bd unreachable"), wantErr: "read beads"},
		{name: "proposed file cannot be written", beads: trainTestBeads(90, 1, true), wantErr: storyPointsProposedName,
			setup: blockAt(storyPointsProposedName)},
		{name: "report cannot be written", beads: trainTestBeads(90, 1, true), wrote: true, wantErr: storyPointsTrainReportName,
			setup: blockAt(storyPointsTrainReportName)},
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

// TestStoryPointsTrainSkipsAndCountsUnreadBeads: a landing that is a lower bound, a
// bead not in the store and a bead with no section text are left out of the fit and
// counted in the report; the rest of the table is fitted.
func TestStoryPointsTrainSkipsAndCountsUnreadBeads(t *testing.T) {
	t.Parallel()
	bead := trainTestBeads(90, 5, true)
	bead[0].bound = "1"
	bead[1].missing = true
	bead[2].noText = true
	fix := newTrainTestFixture(t, bead)
	var out strings.Builder
	c := fix.commands(t, &out, trainTestEmbedder{}, fix.store())
	if err := Run(context.Background(), CommandStoryPointsTrain, c); err != nil {
		t.Fatalf("storypoints-train: %v", err)
	}
	want := "not read: seat class 0, unmeasured 1, bead not in store 1, no Constraints or Acceptance 1, rig without a directory 0"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report = %q, want the line %q", out.String(), want)
	}
	// The 87 beads read are split: the earlier 70 are the fit and the latest 17 the holdout.
	if !strings.Contains(out.String(), "n=70)") {
		t.Errorf("report does not count 70 fitted beads:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "holdout: latest 17 beads") {
		t.Errorf("report does not count 17 holdout beads:\n%s", out.String())
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
			report := storyPointsTrainReport(storypoints.Model{Holdout: tc.holdout, TrainedOn: "fixture"}, trainSkips{}, name, score)
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
	samples, _, err := trainingSamples(context.Background(), env, fix.town, rows, trainTestEmbedder{})
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
