package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// adoptTestWeights is a weights file for the fixture's 3-dim embedder. boxes is
// the shape ridge's slope on acceptance boxes, intercept is the ridge's
// intercept, and the embedding ridge is zero, so every embedding prediction is
// one minute and the
// blend's ranks follow the boxes alone. That makes both models of a case
// monotone in the fixture's minutes, so the command's wiring is what a case
// exercises; the rule's arithmetic is tested in internal/storypoints.
type adoptTestWeights struct {
	boxes     float64
	intercept float64
	dim       int    // zero is trainTestDim
	model     string // empty is trainTestDim's model
}

// JSON is the weights file loadable by storypoints.LoadWeights.
func (w adoptTestWeights) JSON() string {
	dim, model := w.dim, w.model
	if dim == 0 {
		dim = trainTestDim
	}
	if model == "" {
		model = "test-embed"
	}
	return fmt.Sprintf(`{"version":1,
	"shape":{"features":["boxes","paths","pkgs","dlen","ncons","nlab","podeng","tests","nbt","rig_gt","rig_dv","is_haiku"],
	"mean":[0,0,0,0,0,0,0,0,0,0,0,0],"scale":[1,1,1,1,1,1,1,1,1,1,1,1],"coef":[%g,0,0,0,0,0,0,0,0,0,0,0],"intercept":%g,"alpha":1},
	"fits_twenty_minutes_to_minutes_knots":[[0,1],[1,100]],
	"embedding":{"model":%q,"dim":%d,"input":"test input","alpha":1,"intercept":0,"coef":%s}}`,
		w.boxes, w.intercept, model, dim, adoptZeroList(dim))
}

// adoptZeroList is n zero numbers as a JSON list.
func adoptZeroList(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "0"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// adoptWeights loads one of the fixture's weights files.
func adoptWeights(t *testing.T, w adoptTestWeights) storypoints.Weights {
	t.Helper()
	got, err := storypoints.LoadWeights(strings.NewReader(w.JSON()))
	if err != nil {
		t.Fatalf("LoadWeights(%+v): %v", w, err)
	}
	return got
}

// adoptRuntimePath is the file the estimator reads.
func adoptRuntimePath(town string) string {
	return filepath.Join(town, ".runtime", "storypoints", "weights.json")
}

// adoptVersionPath is one numbered version beside the runtime file.
func adoptVersionPath(town string, n int) string {
	return filepath.Join(town, ".runtime", "storypoints", "weights.v"+fmt.Sprint(n)+".json")
}

// writeAt writes data at path, making the directories above it.
func writeAt(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readAt reads path and fails the case when it cannot.
func readAt(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// adoptCommands wires the fixture's town to the adoption step.
func (f trainTestFixture) adoptCommands(out *strings.Builder, base storypoints.Weights, emb storypoints.Embedder, revert bool) Commands {
	return Commands{
		Today: commandToday,
		Wire: func(context.Context) (RealTown, error) {
			return RealTown{Town: f.town, Extract: ExtractEnv{OutDir: f.outDir}}, nil
		},
		Env: Env{
			Town:     f.town,
			ReadFile: os.ReadFile,
			ReadDir:  listDir,
			Beads:    func(context.Context, string) BeadStore { return f.store() },
		},
		Out:               out,
		StoryPointsBase:   &base,
		Embedder:          emb,
		StoryPointsRevert: revert,
	}
}

// TestStoryPointsAdoptWritesTheRuntimeWeights: an adopted proposal becomes the
// runtime file byte for byte, and the file it replaced is kept as version 1.
func TestStoryPointsAdoptWritesTheRuntimeWeights(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(fix.town), previous)
	proposed := adoptTestWeights{boxes: 0.25, intercept: 1}.JSON()
	writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), proposed)

	var out strings.Builder
	c := fix.adoptCommands(&out, adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 1}), trainTestEmbedder{}, false)
	if err := Run(context.Background(), CommandStoryPointsAdopt, c); err != nil {
		t.Fatalf("storypoints-adopt: %v", err)
	}

	if got := readAt(t, adoptRuntimePath(fix.town)); got != proposed {
		t.Errorf("runtime weights = %q, want the proposed file", got)
	}
	if got := readAt(t, adoptVersionPath(fix.town, 1)); got != previous {
		t.Errorf("version 1 = %q, want the weights it replaced", got)
	}
	for _, want := range []string{"adopted:", adoptRuntimePath(fix.town), "weights.v1.json"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("out = %q, want it to name %q", out.String(), want)
		}
	}
}

// TestStoryPointsAdoptRejectsWithoutWriting: a proposal below the bar changes no
// file and prints the rule's reason.
func TestStoryPointsAdoptRejectsWithoutWriting(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), adoptTestWeights{boxes: -0.25, intercept: 1}.JSON())

	var out strings.Builder
	c := fix.adoptCommands(&out, adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 1}), trainTestEmbedder{}, false)
	if err := Run(context.Background(), CommandStoryPointsAdopt, c); err != nil {
		t.Fatalf("storypoints-adopt: %v", err)
	}

	for _, want := range []string{"rejected:", "below the 0.40 bar"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("out = %q, want it to name %q", out.String(), want)
		}
	}
	if _, err := os.Stat(adoptRuntimePath(fix.town)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("weights.json stat = %v, want the rejection to write nothing", err)
	}
	if _, err := os.Stat(filepath.Dir(adoptRuntimePath(fix.town))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("runtime directory stat = %v, want the rejection to write nothing", err)
	}
}

// TestStoryPointsAdoptRevertPopsOneVersionAtATime: each --revert restores the
// newest version and consumes it, and a third finds nothing to restore.
func TestStoryPointsAdoptRevertPopsOneVersionAtATime(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	w0 := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	w1 := adoptTestWeights{boxes: 0.10, intercept: 1}.JSON()
	w2 := adoptTestWeights{boxes: 0.15, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(town), w0)
	writeAt(t, adoptVersionPath(town, 1), w1)
	writeAt(t, adoptVersionPath(town, 2), w2)

	revert := func() (string, error) {
		t.Helper()
		var out strings.Builder
		c := Commands{
			Today:             commandToday,
			Wire:              func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
			Env:               Env{Town: town, ReadFile: os.ReadFile},
			Out:               &out,
			StoryPointsRevert: true,
		}
		err := Run(context.Background(), CommandStoryPointsAdopt, c)
		return out.String(), err
	}
	mustRevert := func() string {
		t.Helper()
		out, err := revert()
		if err != nil {
			t.Fatalf("storypoints-adopt --revert: %v", err)
		}
		return out
	}

	if out := mustRevert(); !strings.Contains(out, "weights.v2.json") {
		t.Errorf("out = %q, want it to name weights.v2.json", out)
	}
	if got := readAt(t, adoptRuntimePath(town)); got != w2 {
		t.Errorf("after one revert the runtime weights are %q, want version 2", got)
	}
	if _, err := os.Stat(adoptVersionPath(town, 2)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("version 2 stat = %v, want it consumed by the revert", err)
	}

	if out := mustRevert(); !strings.Contains(out, "weights.v1.json") {
		t.Errorf("out = %q, want it to name weights.v1.json", out)
	}
	if got := readAt(t, adoptRuntimePath(town)); got != w1 {
		t.Errorf("after two reverts the runtime weights are %q, want version 1", got)
	}
	if _, err := os.Stat(adoptVersionPath(town, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("version 1 stat = %v, want it consumed by the revert", err)
	}

	if out, err := revert(); err == nil || !strings.Contains(err.Error(), "no earlier version") {
		t.Errorf("third revert = (%q, %v), want an error saying there is nothing left to restore", out, err)
	}
	if got := readAt(t, adoptRuntimePath(town)); got != w1 {
		t.Errorf("after the third revert the runtime weights are %q, want them unchanged", got)
	}
}

// TestStoryPointsAdoptRevertWithNothingToRestoreFails: --revert with no version
// beside the runtime file changes nothing and is an error, not a success the
// hour run cannot tell from a restore.
func TestStoryPointsAdoptRevertWithNothingToRestoreFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	live := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(town), live)

	var out strings.Builder
	c := Commands{
		Today:             commandToday,
		Wire:              func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Env:               Env{Town: town, ReadFile: os.ReadFile},
		Out:               &out,
		StoryPointsRevert: true,
	}
	err := Run(context.Background(), CommandStoryPointsAdopt, c)
	if err == nil || !strings.Contains(err.Error(), "no earlier version") {
		t.Fatalf("err = %v, want it to say there is nothing to restore", err)
	}
	if got := readAt(t, adoptRuntimePath(town)); got != live {
		t.Errorf("runtime weights = %q, want them unchanged", got)
	}
	if out.Len() != 0 {
		t.Errorf("out = %q, want no receipt for a revert that changed nothing", out.String())
	}
}

// TestStoryPointsAdoptKeepsEightNewestVersions: adoption adds a version and
// deletes the oldest ones past the eighth, and a name that is not a version is
// neither counted nor deleted.
func TestStoryPointsAdoptKeepsEightNewestVersions(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	previous := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(fix.town), previous)
	for n := 1; n <= 10; n++ {
		writeAt(t, adoptVersionPath(fix.town, n), adoptTestWeights{boxes: 0.01 * float64(n), intercept: 1}.JSON())
	}
	notAVersion := filepath.Join(filepath.Dir(adoptRuntimePath(fix.town)), "weights.vX.json")
	writeAt(t, notAVersion, "not a version")
	writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), adoptTestWeights{boxes: 0.25, intercept: 1}.JSON())

	var out strings.Builder
	c := fix.adoptCommands(&out, adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 1}), trainTestEmbedder{}, false)
	if err := Run(context.Background(), CommandStoryPointsAdopt, c); err != nil {
		t.Fatalf("storypoints-adopt: %v", err)
	}

	dir := filepath.Dir(adoptRuntimePath(fix.town))
	for n := 1; n <= 3; n++ {
		if _, err := os.Stat(adoptVersionPath(fix.town, n)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("version %d stat = %v, want the oldest versions deleted", n, err)
		}
	}
	for n := 4; n <= 11; n++ {
		if _, err := os.Stat(adoptVersionPath(fix.town, n)); err != nil {
			t.Errorf("version %d: %v, want the newest eight kept", n, err)
		}
	}
	if got := readAt(t, notAVersion); got != "not a version" {
		t.Errorf("weights.vX.json = %q, want a name that is not a version left alone", got)
	}
	if _, err := os.Stat(adoptRuntimePath(fix.town)); err != nil {
		t.Errorf("runtime weights: %v, want the adopted proposal written", err)
	}
	if !strings.Contains(out.String(), "weights.v11.json") {
		t.Errorf("out = %q, want it to name the version the previous weights were kept as", out.String())
	}
	if got, want := len(versionFiles(t, dir)), 8+2; got != want {
		t.Errorf("%d files in the runtime directory, want %d (8 versions, weights.json and weights.vX.json)", got, want)
	}
}

// versionFiles lists the files in dir, failing the case when it cannot.
func versionFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// adoptWrongDimEmbedder embeds every bead one number longer than the fixture's
// model, so the blend refuses the vector.
type adoptWrongDimEmbedder struct{}

func (adoptWrongDimEmbedder) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, trainTestDim+1), nil
}

// TestStoryPointsAdoptRefusals: each input the step cannot decide from stops with
// the reason named and writes no file, and a holdout too small to measure is a
// printed rejection.
func TestStoryPointsAdoptRefusals(t *testing.T) {
	t.Parallel()
	strong := adoptTestWeights{boxes: 0.25, intercept: 1}.JSON()
	weak := adoptTestWeights{boxes: 0.05, intercept: 1}
	overrideLandings := func(header string) func(*testing.T, trainTestFixture) {
		return func(t *testing.T, fix trainTestFixture) {
			t.Helper()
			var b strings.Builder
			b.WriteString(header + "\n")
			for _, bd := range fix.beads {
				b.WriteString(bd.id + ",gastown\n")
			}
			writeAt(t, filepath.Join(fix.outDir, landingsName), b.String())
		}
	}
	sameLandingTime := func(t *testing.T, fix trainTestFixture) {
		t.Helper()
		beads := trainTestBeads(90, 1, true)
		for i := range beads {
			if i > 0 {
				beads[i].sameTimeAsPrev = true
			}
		}
		// Every landing at the first bead's time, so the split cannot separate a
		// holdout from the fit.
		replace := newTrainTestFixture(t, beads)
		writeAt(t, filepath.Join(fix.outDir, landingsName), readAt(t, filepath.Join(replace.outDir, landingsName)))
	}
	cases := []struct {
		name string
		// setup mutates the fixture and returns the runtime weights it seeded, or
		// "" when the case leaves the town with no runtime file.
		setup    func(*testing.T, trainTestFixture) string
		embedder storypoints.Embedder
		wantErr  string
		wantOut  string
	}{
		{
			name: "no proposed file",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				os.Remove(filepath.Join(fix.outDir, storyPointsProposedName))
				return ""
			},
			wantErr: CommandStoryPointsTrain,
		},
		{
			name: "both proposed names",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName+storyPointsRejectedSuffix), strong)
				return ""
			},
			wantErr: "ambiguous",
		},
		{
			name: "proposed file does not load",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), "{")
				return ""
			},
			wantErr: "proposed weights",
		},
		{
			name: "table from an older extract",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				overrideLandings("bead,rig")(t, fix)
				return ""
			},
			wantErr: "no polecat_work column",
		},
		{
			name: "runtime weights do not load",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				writeAt(t, adoptRuntimePath(fix.town), "{")
				return "{"
			},
			wantErr: "runtime weights",
		},
		{
			name: "runtime weights embed another dimension",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				seeded := adoptTestWeights{boxes: 0.05, intercept: 1, dim: trainTestDim + 1}.JSON()
				writeAt(t, adoptRuntimePath(fix.town), seeded)
				return seeded
			},
			wantErr: "cannot compare",
		},
		{
			name: "runtime weights embed with another model",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				seeded := adoptTestWeights{boxes: 0.05, intercept: 1, model: "other-embed"}.JSON()
				writeAt(t, adoptRuntimePath(fix.town), seeded)
				return seeded
			},
			wantErr: "not comparable",
		},
		{
			name: "proposed path cannot be read",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				path := filepath.Join(fix.outDir, storyPointsProposedName)
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove the proposed file: %v", err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("make the proposed path a directory: %v", err)
				}
				return ""
			},
			wantErr: "read",
		},
		{
			name: "runtime path cannot be read",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				if err := os.MkdirAll(adoptRuntimePath(fix.town), 0o755); err != nil {
					t.Fatalf("make the runtime path a directory: %v", err)
				}
				return ""
			},
			wantErr: "read",
		},
		{
			name:     "embedder fails",
			embedder: trainTestEmbedder{fail: true},
			wantErr:  "embedder unreachable",
		},
		{
			name: "holdout too small to measure",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				sameLandingTime(t, fix)
				return ""
			},
			wantOut: "fewer than the 10",
		},
		{
			name: "no extract holds a landings table",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				if err := os.Remove(filepath.Join(fix.outDir, landingsName)); err != nil {
					t.Fatalf("remove the landings table: %v", err)
				}
				return ""
			},
			wantErr: CommandExtract,
		},
		{
			name: "landings table does not parse",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				writeAt(t, filepath.Join(fix.outDir, landingsName), "bead,rig,polecat_work\ngt-t000\n")
				return ""
			},
			wantErr: "landings.csv",
		},
		{
			name:     "embedder returns another model's vector length",
			embedder: adoptWrongDimEmbedder{},
			wantErr:  "embedding has",
		},
		{
			name: "weights in use overflow the shape prediction",
			setup: func(t *testing.T, fix trainTestFixture) string {
				t.Helper()
				seeded := adoptTestWeights{boxes: 0.05, intercept: 1000}.JSON()
				writeAt(t, adoptRuntimePath(fix.town), seeded)
				return seeded
			},
			wantErr: "shape input",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), strong)
			seeded := ""
			if tc.setup != nil {
				seeded = tc.setup(t, fix)
			}
			emb := tc.embedder
			if emb == nil {
				emb = trainTestEmbedder{}
			}
			var out strings.Builder
			c := fix.adoptCommands(&out, adoptWeights(t, weak), emb, false)
			err := Run(context.Background(), CommandStoryPointsAdopt, c)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("no error, want one naming %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to name %q", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("err = %v, want a printed decision", err)
				}
				if !strings.Contains(out.String(), tc.wantOut) {
					t.Errorf("out = %q, want it to name %q", out.String(), tc.wantOut)
				}
			}
			// Every case here must leave the runtime file as it found it.
			info, statErr := os.Stat(adoptRuntimePath(fix.town))
			switch {
			case errors.Is(statErr, fs.ErrNotExist):
			case statErr != nil:
				t.Fatalf("stat weights.json: %v", statErr)
			case !info.Mode().IsRegular():
				// A case made the path a directory; that is not a write.
			case seeded == "":
				t.Errorf("weights.json written, want the case to write nothing")
			case readAt(t, adoptRuntimePath(fix.town)) != seeded:
				t.Errorf("runtime weights changed, want them untouched")
			}
			if _, err := os.Stat(adoptVersionPath(fix.town, 1)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("version 1 stat = %v, want no version written", err)
			}
		})
	}
}

// TestStoryPointsBlendRefusesWhatItCannotScore: the blend cannot be taken from
// a Weights with no shape model, nor from a sample whose vector the weights'
// embedding ridge cannot read, and the step reports each rather than a minute no
// rule can rank.
func TestStoryPointsBlendRefusesWhatItCannotScore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		weights storypoints.Weights
		sample  storypoints.Sample
		wantErr string
	}{
		{
			name:    "weights that are not loaded",
			weights: storypoints.Weights{},
			wantErr: "weights are not loaded",
		},
		{
			name:    "a vector the ridge cannot read",
			weights: adoptWeights(t, adoptTestWeights{boxes: 0.05, intercept: 1}),
			sample:  storypoints.Sample{Vector: make([]float32, trainTestDim-1)},
			wantErr: "embedding has 2 numbers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := storyPointsBlend(tc.weights, tc.sample); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("storyPointsBlend = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestStoryPointsAdoptStoreRefusals drives every write-side refusal: a listing,
// a write or a deletion the step cannot make, and a destination a file cannot
// replace. The listing, write and delete refusals go through the store seam,
// because a directory mode bit is no refusal to a test run as root; the rest
// use the host filesystem, whose refusals come from the shape of the path.
func TestStoryPointsAdoptStoreRefusals(t *testing.T) {
	t.Parallel()
	strong := adoptTestWeights{boxes: 0.25, intercept: 1}.JSON()
	weak := adoptTestWeights{boxes: 0.05, intercept: 1}
	fail := errors.New("the store refused")
	nineVersions := make([]DirEntry, 0, 9)
	for n := 1; n <= 9; n++ {
		nineVersions = append(nineVersions, DirEntry{Name: storyPointsVersionName("weights", n)})
	}
	cases := []struct {
		name  string
		setup func(*testing.T, trainTestFixture)
		store storyPointsStore
		// revert runs the step with StoryPointsRevert.
		revert  bool
		wantErr string
		// wantRuntime is what the runtime file holds after the run: the seeded
		// weights when the step refuses before writing it, the proposal when it
		// refuses after, and "" when no regular file may be there.
		wantRuntime string
	}{
		{
			name: "the versions cannot be listed for the snapshot",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				writeAt(t, adoptRuntimePath(fix.town), weak.JSON())
			},
			store:       storyPointsStore{List: func(string) ([]DirEntry, error) { return nil, fail }},
			wantErr:     "list",
			wantRuntime: weak.JSON(),
		},
		{
			name: "the versions cannot be listed for retention",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				writeAt(t, adoptRuntimePath(fix.town), weak.JSON())
			},
			store: func() storyPointsStore {
				listings := 0
				return storyPointsStore{List: func(dir string) ([]DirEntry, error) {
					listings++
					if listings > 1 {
						return nil, fail
					}
					return listDir(dir)
				}}
			}(),
			wantErr:     "list",
			wantRuntime: strong,
		},
		{
			name:        "an old version cannot be removed",
			store:       storyPointsStore{List: func(string) ([]DirEntry, error) { return nineVersions, nil }, Remove: func(string) error { return fail }},
			wantErr:     "remove",
			wantRuntime: strong,
		},
		{
			name:        "the runtime weights cannot be written",
			store:       storyPointsStore{Write: func(string, string, func(io.Writer) error) error { return fail }},
			wantErr:     "write",
			wantRuntime: "",
		},
		{
			name: "version target cannot be replaced",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				writeAt(t, adoptRuntimePath(fix.town), weak.JSON())
				if err := os.Mkdir(adoptVersionPath(fix.town, 1), 0o755); err != nil {
					t.Fatalf("block version 1: %v", err)
				}
			},
			wantErr:     "write",
			wantRuntime: weak.JSON(),
		},
		{
			name: "runtime directory is a dangling symlink",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				dir := filepath.Dir(adoptRuntimePath(fix.town))
				if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
					t.Fatalf("make the runtime root: %v", err)
				}
				if err := os.Symlink(filepath.Join(filepath.Dir(dir), "missing"), dir); err != nil {
					t.Fatalf("symlink the runtime directory: %v", err)
				}
			},
			wantErr:     "create",
			wantRuntime: "",
		},
		{
			name: "revert cannot list versions",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				writeAt(t, filepath.Dir(adoptRuntimePath(fix.town)), "not a directory")
			},
			revert:      true,
			wantErr:     "list",
			wantRuntime: "",
		},
		{
			name: "revert cannot read the newest version",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				writeAt(t, adoptRuntimePath(fix.town), weak.JSON())
				if err := os.Symlink(filepath.Join(filepath.Dir(adoptRuntimePath(fix.town)), "gone"), adoptVersionPath(fix.town, 1)); err != nil {
					t.Fatalf("symlink version 1: %v", err)
				}
			},
			revert:      true,
			wantErr:     "read",
			wantRuntime: weak.JSON(),
		},
		{
			name: "revert cannot replace the runtime file",
			setup: func(t *testing.T, fix trainTestFixture) {
				t.Helper()
				if err := os.MkdirAll(adoptRuntimePath(fix.town), 0o755); err != nil {
					t.Fatalf("make weights.json a directory: %v", err)
				}
				writeAt(t, adoptVersionPath(fix.town, 1), weak.JSON())
			},
			revert:      true,
			wantErr:     "restore",
			wantRuntime: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), strong)
			if tc.setup != nil {
				tc.setup(t, fix)
			}
			var out strings.Builder
			c := fix.adoptCommands(&out, adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 1}), trainTestEmbedder{}, tc.revert)
			c.storyPointsStore = tc.store
			err := Run(context.Background(), CommandStoryPointsAdopt, c)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
			}
			got, readErr := os.ReadFile(adoptRuntimePath(fix.town))
			if tc.wantRuntime == "" {
				if readErr == nil {
					t.Errorf("runtime weights = %q, want no regular file there", got)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("read the runtime weights: %v", readErr)
			}
			if string(got) != tc.wantRuntime {
				t.Errorf("runtime weights = %q, want %q", got, tc.wantRuntime)
			}
		})
	}
}

// TestStoryPointsAdoptLeavesANonCanonicalVersionAlone: weights.v03.json is not
// a version this step keeps, so neither the retention count nor --revert may
// act on it: a revert that read it as 3 would restore from weights.v3.json,
// which need not exist.
func TestStoryPointsAdoptLeavesANonCanonicalVersionAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	live := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	older := adoptTestWeights{boxes: 0.10, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(town), live)
	writeAt(t, filepath.Join(filepath.Dir(adoptRuntimePath(town)), "weights.v03.json"), older)

	var out strings.Builder
	c := Commands{
		Today:             commandToday,
		Wire:              func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Env:               Env{Town: town, ReadFile: os.ReadFile},
		Out:               &out,
		StoryPointsRevert: true,
	}
	err := Run(context.Background(), CommandStoryPointsAdopt, c)
	if err == nil || !strings.Contains(err.Error(), "no earlier version") {
		t.Fatalf("err = %v, want the padded name read as no version at all", err)
	}
	if got := readAt(t, filepath.Join(filepath.Dir(adoptRuntimePath(town)), "weights.v03.json")); got != older {
		t.Errorf("weights.v03.json = %q, want it left alone", got)
	}
	if got := readAt(t, adoptRuntimePath(town)); got != live {
		t.Errorf("runtime weights = %q, want them unchanged", got)
	}
}

// TestStoryPointsHoldoutIsTheRetrainSplit: the adoption step repeats the
// retrain's split because the retrain does not write its holdout down. Two
// copies of one rule drift, so this holds them to the same holdout on the same
// samples, including the boundary that advances past a landing time shared with
// the sample before it.
func TestStoryPointsHoldoutIsTheRetrainSplit(t *testing.T) {
	t.Parallel()
	base := adoptWeights(t, adoptTestWeights{boxes: 0.05, intercept: 1})
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	samples := make([]storypoints.Sample, 0, 120)
	for i := 0; i < 120; i++ {
		landed := start.Add(time.Duration(i) * time.Hour)
		// Seven samples share one landing time across the 80% boundary, so a
		// split that only rounds puts one of them on each side.
		if i >= 94 && i <= 100 {
			landed = start.Add(94 * time.Hour)
		}
		samples = append(samples, storypoints.Sample{
			Vector:   []float32{float32(i % 5), float32(i % 3), float32(i % 7)},
			Minutes:  float64(i%17) + 1,
			LandedAt: landed,
		})
	}
	model, err := storypoints.Retrain(base, samples)
	if err != nil {
		t.Fatalf("Retrain: %v", err)
	}
	holdout, err := storyPointsHoldout(samples, base, base)
	if err != nil {
		t.Fatalf("storyPointsHoldout: %v", err)
	}
	if len(holdout) != len(model.Holdout) {
		t.Fatalf("the adoption holdout has %d beads, the retrain's %d", len(holdout), len(model.Holdout))
	}
	for i := range holdout {
		if holdout[i].Actual != model.Holdout[i].Actual {
			t.Fatalf("holdout bead %d has %v minutes, the retrain's has %v: the two splits differ",
				i, holdout[i].Actual, model.Holdout[i].Actual)
		}
	}
}

// TestStoryPointsSharedBuilderNamesNoStep: trainingSamples fits both the retrain
// and the adoption step, so its errors name no step; the wrapper that runs the
// step names it, exactly once.
func TestStoryPointsSharedBuilderNamesNoStep(t *testing.T) {
	t.Parallel()
	fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
	data, err := os.ReadFile(filepath.Join(fix.outDir, landingsName))
	if err != nil {
		t.Fatalf("read landings: %v", err)
	}
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	env := Env{Town: fix.town, ReadFile: os.ReadFile, Beads: func(context.Context, string) BeadStore { return fix.store() }}
	_, _, err = trainingSamples(context.Background(), env, fix.town, rows, storypoints.Default(), trainTestEmbedder{fail: true})
	if err == nil {
		t.Fatal("trainingSamples returned no error with a failing embedder")
	}
	for _, step := range []string{CommandStoryPointsTrain, CommandStoryPointsAdopt} {
		if strings.Contains(err.Error(), step) {
			t.Errorf("trainingSamples error %q names %s, want the caller to", err, step)
		}
	}

	writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), adoptTestWeights{boxes: 0.25, intercept: 1}.JSON())
	var out strings.Builder
	c := fix.adoptCommands(&out, adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 1}), trainTestEmbedder{fail: true}, false)
	err = Run(context.Background(), CommandStoryPointsAdopt, c)
	if err == nil {
		t.Fatal("storypoints-adopt returned no error with a failing embedder")
	}
	if got := strings.Count(err.Error(), CommandStoryPointsAdopt); got != 1 {
		t.Errorf("error %q names %s %d times, want once", err, CommandStoryPointsAdopt, got)
	}
	if got := strings.Count(err.Error(), CommandStoryPointsTrain); got != 0 {
		t.Errorf("error %q names %s %d times, want the step that ran", err, CommandStoryPointsTrain, got)
	}
}

// TestStoryPointsAdoptRefusesNoTownRoot: both modes need the town to name the
// runtime weights path.
func TestStoryPointsAdoptRefusesNoTownRoot(t *testing.T) {
	t.Parallel()
	for _, revert := range []bool{false, true} {
		t.Run(fmt.Sprintf("revert=%v", revert), func(t *testing.T) {
			t.Parallel()
			fix := newTrainTestFixture(t, trainTestBeads(90, 1, true))
			writeAt(t, filepath.Join(fix.outDir, storyPointsProposedName), adoptTestWeights{boxes: 0.25, intercept: 1}.JSON())
			var out strings.Builder
			c := Commands{
				Today: commandToday,
				Wire: func(context.Context) (RealTown, error) {
					return RealTown{Extract: ExtractEnv{OutDir: fix.outDir}}, nil
				},
				Env:               Env{ReadFile: os.ReadFile},
				Out:               &out,
				Embedder:          trainTestEmbedder{},
				StoryPointsRevert: revert,
			}
			err := Run(context.Background(), CommandStoryPointsAdopt, c)
			if err == nil || !strings.Contains(err.Error(), "no town root") {
				t.Fatalf("err = %v, want it to name the missing town root", err)
			}
		})
	}
}

// TestStoryPointsAdoptRevertRefusesAVersionThatDoesNotLoad: a corrupt version is
// reported and left in place, so the runtime file is not replaced by it.
func TestStoryPointsAdoptRevertRefusesAVersionThatDoesNotLoad(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	live := adoptTestWeights{boxes: 0.05, intercept: 1}.JSON()
	writeAt(t, adoptRuntimePath(town), live)
	writeAt(t, adoptVersionPath(town, 3), "{")

	var out strings.Builder
	c := Commands{
		Today:             commandToday,
		Wire:              func(context.Context) (RealTown, error) { return RealTown{Town: town}, nil },
		Env:               Env{Town: town, ReadFile: os.ReadFile},
		Out:               &out,
		StoryPointsRevert: true,
	}
	err := Run(context.Background(), CommandStoryPointsAdopt, c)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v, want it to name the version that does not load", err)
	}
	if got := readAt(t, adoptRuntimePath(town)); got != live {
		t.Errorf("runtime weights = %q, want them unchanged", got)
	}
	if got := readAt(t, adoptVersionPath(town, 3)); got != "{" {
		t.Errorf("version 3 = %q, want the corrupt file left for a person to inspect", got)
	}
}
