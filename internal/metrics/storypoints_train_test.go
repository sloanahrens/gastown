package metrics

import (
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
	for i, b := range beadsIn {
		landed := start.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		if b.badTime {
			landed = "last tuesday"
		}
		rows = append(rows, Row{
			"bead":                b.id,
			"rig":                 "gastown",
			"landed_at":           landed,
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
	if !strings.Contains(out.String(), "n=87\n") {
		t.Errorf("report does not count 87 fitted beads:\n%s", out.String())
	}
}
