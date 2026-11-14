package storypoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
)

const initialWeightsPath = "../../docs/plans/2026-11-11-story-points-initial-weights.json"

func TestDefaultLoadsEmbeddedWeights(t *testing.T) {
	t.Parallel()
	want := initialShapeMean(t, "is_haiku")
	w := Default()
	if got, ok := w.ShapeMean("is_haiku"); !ok || got != want {
		t.Errorf("ShapeMean(is_haiku) = %v, %v; want %v, true", got, ok, want)
	}
	if _, ok := w.ShapeMean("not_a_feature"); ok {
		t.Errorf("ShapeMean(not_a_feature) reports ok; want false")
	}
}

func TestEmbeddedWeightsAreTheInitialFile(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(initialWeightsPath)
	if err != nil {
		t.Fatalf("read initial weights: %v", err)
	}
	if !bytes.Equal(embeddedWeights, want) {
		t.Errorf("embedded weights.json differs from %s: %d bytes, want %d identical bytes",
			initialWeightsPath, len(embeddedWeights), len(want))
	}
}

func TestShapeMinutesMatchFixtures(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(shapeFixturesPath)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures []struct {
		Bead             string  `json:"bead"`
		Rig              string  `json:"rig"`
		SeatClass        string  `json:"seat_class"`
		Text             string  `json:"text"`
		PredictedMinutes float64 `json:"predicted_minutes"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixtures) != 6 {
		t.Fatalf("fixtures = %d beads, want 6", len(fixtures))
	}
	w := Default()
	for _, fx := range fixtures {
		t.Run(fx.Bead, func(t *testing.T) {
			t.Parallel()
			seat := SeatFlash
			if fx.SeatClass == "haiku" {
				seat = SeatHaiku
			}
			got, err := w.ShapeMinutes(Features(fx.Text, fx.Rig, seat, w))
			if err != nil {
				t.Fatalf("ShapeMinutes: %v", err)
			}
			if math.Abs(got-fx.PredictedMinutes) > 0.01 {
				t.Errorf("ShapeMinutes = %v, want %v within 0.01", got, fx.PredictedMinutes)
			}
		})
	}
}

func TestShapeMinutesIsExpOfLinearPredictor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		shape Shape
		mut   func(f *weightsFile)
		want  float64
	}{
		{
			name:  "intercept alone",
			shape: Shape{Boxes: 3},
			mut: func(f *weightsFile) {
				clear(f.Shape.Coef)
				f.Shape.Intercept = math.Log(7)
			},
			want: 7,
		},
		{
			name:  "one term: coef * (x - mean) / scale",
			shape: Shape{Boxes: 3},
			mut: func(f *weightsFile) {
				clear(f.Shape.Coef)
				f.Shape.Intercept = 0
				f.Shape.Coef[0] = 0.5
				f.Shape.Mean[0] = 1
				f.Shape.Scale[0] = 2
			},
			want: math.Exp(0.5),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := loadWeightsFrom(t, weightsWith(t, tc.mut))
			got, err := w.ShapeMinutes(tc.shape)
			if err != nil {
				t.Fatalf("ShapeMinutes: %v", err)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("ShapeMinutes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShapeMinutesZeroWeightsReturnsError(t *testing.T) {
	t.Parallel()
	got, err := (Weights{}).ShapeMinutes(Shape{Boxes: 3})
	if !errors.Is(err, ErrNoWeights) {
		t.Fatalf("ShapeMinutes(zero Weights) = %v, %v; want ErrNoWeights", got, err)
	}
}

func TestLoadWeightsAcceptsTheEmbeddedFile(t *testing.T) {
	t.Parallel()
	if _, err := LoadWeights(bytes.NewReader(embeddedWeights)); err != nil {
		t.Errorf("LoadWeights(embedded) = %v, want nil", err)
	}
	// A re-marshalled copy of the typed file must load too, so the rejection
	// rows below fail for their one change and not for the round trip.
	if _, err := LoadWeights(bytes.NewReader(weightsWith(t, func(*weightsFile) {}))); err != nil {
		t.Errorf("LoadWeights(round trip) = %v, want nil", err)
	}
}

func TestLoadWeightsRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data func(t *testing.T) []byte
	}{
		{"feature name changed", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Features[0] = "box" })
		}},
		{"feature dropped", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Features = f.Shape.Features[:11] })
		}},
		{"coefficient list too short", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Coef = f.Shape.Coef[:11] })
		}},
		{"mean list too long", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Mean = append(f.Shape.Mean, 0) })
		}},
		{"zero scale", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Scale[3] = 0 })
		}},
		{"negative scale", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Scale[3] = -1 })
		}},
		{"number out of float64 range", func(t *testing.T) []byte {
			return replaceOnce(t, `"intercept": 2.16992`, `"intercept": 1e400`)
		}},
		{"knot table unsorted", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) {
				last := len(f.Knots) - 1
				f.Knots[0], f.Knots[last] = f.Knots[last], f.Knots[0]
			})
		}},
		{"knot probability above 1", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots[0][0] = 1.5 })
		}},
		{"knot minutes zero", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots[1][1] = 0 })
		}},
		{"knot row without two numbers", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots[2] = []float64{0.1} })
		}},
		{"knot table empty", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots = nil })
		}},
		{"embedding dim differs from its coefficients", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Embedding.Dim = 2559 })
		}},
		{"embedding model name empty", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Embedding.Model = "" })
		}},
		{"unknown version", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Version = 2 })
		}},
		{"data after the JSON object", func(t *testing.T) []byte {
			return append(bytes.Clone(embeddedWeights), "{}"...)
		}},
		{"not JSON", func(t *testing.T) []byte { return []byte("not json") }},
		{"empty input", func(t *testing.T) []byte { return nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadWeights(bytes.NewReader(tc.data(t))); err == nil {
				t.Errorf("LoadWeights accepted the file; want an error")
			}
		})
	}
}

// TestLoadWeightsRejectsBareNaN pins why a NaN in the file is refused:
// encoding/json cannot represent NaN, so the decoder stops on the bare literal
// as a syntax error before any NaN check could run. The assertion is on that
// observed reason, not on a NaN check the code does not make.
func TestLoadWeightsRejectsBareNaN(t *testing.T) {
	t.Parallel()
	data := replaceOnce(t, `"intercept": 2.16992`, `"intercept": NaN`)
	var syntax *json.SyntaxError
	if _, err := LoadWeights(bytes.NewReader(data)); !errors.As(err, &syntax) {
		t.Fatalf("LoadWeights(bare NaN) = %v, want a JSON syntax error", err)
	}
}

// runtimeTown is the town root the loader tests ask about, and
// runtimeWeightsPath is the runtime file the path must name under it.
const (
	runtimeTown        = "/town"
	runtimeWeightsPath = "/town/.runtime/storypoints/weights.json"
)

// fakeRuntime is the reader and logger a LoadTownWeights test supplies: read
// serves data or readErr for the path it is given, and logf records the lines
// the loader wrote.
type fakeRuntime struct {
	data    []byte
	readErr error
	paths   []string
	logged  []string
}

func (f *fakeRuntime) read(path string) ([]byte, error) {
	f.paths = append(f.paths, path)
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.data, nil
}

func (f *fakeRuntime) logf(format string, args ...any) {
	f.logged = append(f.logged, fmt.Sprintf(format, args...))
}

func TestRuntimeWeightsPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		town string
		want string
	}{
		{"a town root", runtimeTown, runtimeWeightsPath},
		{"no town root", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RuntimeWeightsPath(tc.town); got != tc.want {
				t.Errorf("RuntimeWeightsPath(%q) = %q, want %q", tc.town, got, tc.want)
			}
		})
	}
}

// TestLoadTownWeightsUsesTheRuntimeFile checks the runtime model's answer, not
// only its label: the intercept is moved by one, so the shape minutes are e
// times the embedded model's.
func TestLoadTownWeightsUsesTheRuntimeFile(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{data: weightsWith(t, func(f *weightsFile) { f.Shape.Intercept++ })}
	got := loadTownWeights(runtimeTown, fake.read, fake.logf)

	if want := "runtime v1"; got.Version() != want {
		t.Fatalf("Version() = %q, want %q", got.Version(), want)
	}
	shape := Shape{Boxes: 3}
	embedded, err := Default().ShapeMinutes(shape)
	if err != nil {
		t.Fatalf("embedded ShapeMinutes: %v", err)
	}
	runtime, err := got.ShapeMinutes(shape)
	if err != nil {
		t.Fatalf("runtime ShapeMinutes: %v", err)
	}
	if math.Abs(runtime/embedded-math.E) > 1e-9 {
		t.Fatalf("runtime ShapeMinutes = %v, %v times the embedded model; want e times", runtime, runtime/embedded)
	}
	if !slices.Equal(fake.paths, []string{runtimeWeightsPath}) {
		t.Fatalf("read paths = %v, want %s", fake.paths, runtimeWeightsPath)
	}
	if len(fake.logged) != 0 {
		t.Fatalf("logged = %v, want no line for a valid file", fake.logged)
	}
}

func TestLoadTownWeightsSkipsAMissingFile(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{readErr: fs.ErrNotExist}
	got := loadTownWeights(runtimeTown, fake.read, fake.logf)

	if want := "embedded v1"; got.Version() != want {
		t.Fatalf("Version() = %q, want %q", got.Version(), want)
	}
	if len(fake.logged) != 0 {
		t.Fatalf("logged = %v, want no line for a town that adopted no model", fake.logged)
	}
}

// TestLoadTownWeightsFallsBackAndReports drives every way a runtime file is
// refused: each falls back to the embedded weights and writes exactly one line
// naming the file and the reason.
func TestLoadTownWeightsFallsBackAndReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fake       *fakeRuntime
		wantReason string
	}{
		{
			name:       "unreadable file",
			fake:       &fakeRuntime{readErr: fmt.Errorf("open %s: %w", runtimeWeightsPath, fs.ErrPermission)},
			wantReason: "permission denied",
		},
		{
			name:       "not JSON",
			fake:       &fakeRuntime{data: []byte("not json")},
			wantReason: "decode weights",
		},
		{
			name:       "fails validation",
			fake:       &fakeRuntime{data: weightsWith(t, func(f *weightsFile) { f.Version = 2 })},
			wantReason: "weights version 2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := loadTownWeights(runtimeTown, tc.fake.read, tc.fake.logf)

			if want := "embedded v1"; got.Version() != want {
				t.Fatalf("Version() = %q, want %q", got.Version(), want)
			}
			if len(tc.fake.logged) != 1 {
				t.Fatalf("logged = %q, want exactly one line", tc.fake.logged)
			}
			line := tc.fake.logged[0]
			if !strings.Contains(line, runtimeWeightsPath) || !strings.Contains(line, tc.wantReason) {
				t.Fatalf("logged line %q does not name the file and the reason %q", line, tc.wantReason)
			}
		})
	}
}

func TestLoadTownWeightsWithNoTownRootReadsNoFile(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{data: weightsWith(t, func(*weightsFile) {})}
	got := loadTownWeights("", fake.read, fake.logf)

	if want := "embedded v1"; got.Version() != want {
		t.Fatalf("Version() = %q, want %q", got.Version(), want)
	}
	if len(fake.paths) != 0 {
		t.Fatalf("read paths = %v, want none", fake.paths)
	}
	if len(fake.logged) != 0 {
		t.Fatalf("logged = %v, want none", fake.logged)
	}
}

// TestShapeMeanWithoutShapeLists checks a Weights with no shape model answers
// no mean instead of reading past its empty list.
func TestShapeMeanWithoutShapeLists(t *testing.T) {
	t.Parallel()
	if mean, ok := (Weights{}).ShapeMean("is_haiku"); ok {
		t.Errorf("ShapeMean(is_haiku) on the zero Weights = %v, true; want false", mean)
	}
}

// TestTownWeightsLoaderLoadsEachTownOnce checks a town's file is read once and
// its weights reused, while a second town gets its own load: the one line a
// rejected file is reported in cannot repeat per estimate.
func TestTownWeightsLoaderLoadsEachTownOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{readErr: fs.ErrNotExist}
	l := NewTownWeightsLoader(fake.read, fake.logf)
	for i := 0; i < 3; i++ {
		if got := l.Load(runtimeTown); got.Version() != "embedded v1" {
			t.Fatalf("Load(%q) = %q, want the embedded weights", runtimeTown, got.Version())
		}
	}
	if want := []string{runtimeWeightsPath}; !slices.Equal(fake.paths, want) {
		t.Fatalf("read paths = %v, want %v", fake.paths, want)
	}
	l.Load("/other")
	if len(fake.paths) != 2 || !strings.HasPrefix(fake.paths[1], "/other/") {
		t.Fatalf("read paths = %v, want a second read under /other", fake.paths)
	}
}

// TestTownWeightsLoaderKeepsTheRuntimeModel checks the cached value is the
// runtime model, so the second call cannot fall back to the embedded weights.
func TestTownWeightsLoaderKeepsTheRuntimeModel(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{data: weightsWith(t, func(f *weightsFile) { f.Shape.Intercept++ })}
	l := NewTownWeightsLoader(fake.read, fake.logf)
	for i := 0; i < 2; i++ {
		if got := l.Load(runtimeTown); got.Version() != "runtime v1" {
			t.Fatalf("Load call %d = %q, want runtime v1", i+1, got.Version())
		}
	}
	if len(fake.paths) != 1 {
		t.Fatalf("read paths = %v, want one read", fake.paths)
	}
}

// TestTownWeightsLoaderReportsARejectedFileOnce checks the fallback line is
// written on the load that rejects the file, not on every call.
func TestTownWeightsLoaderReportsARejectedFileOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeRuntime{data: []byte("not json")}
	l := NewTownWeightsLoader(fake.read, fake.logf)
	for i := 0; i < 2; i++ {
		if got := l.Load(runtimeTown); got.Version() != "embedded v1" {
			t.Fatalf("Load call %d = %q, want the embedded weights", i+1, got.Version())
		}
	}
	if len(fake.logged) != 1 {
		t.Fatalf("logged = %q, want exactly one line", fake.logged)
	}
	if !strings.Contains(fake.logged[0], runtimeWeightsPath) {
		t.Fatalf("logged line %q does not name the file", fake.logged[0])
	}
}

func TestWeightsVersionNamesTheSource(t *testing.T) {
	t.Parallel()
	town := func() Weights {
		fake := &fakeRuntime{data: weightsWith(t, func(*weightsFile) {})}
		return loadTownWeights(runtimeTown, fake.read, fake.logf)
	}()
	loaded, err := LoadWeights(bytes.NewReader(embeddedWeights))
	if err != nil {
		t.Fatalf("LoadWeights(embedded): %v", err)
	}
	tests := []struct {
		name string
		w    Weights
		want string
	}{
		{"the embedded weights", Default(), "embedded v1"},
		{"a town's runtime file", town, "runtime v1"},
		{"bytes a caller supplied", loaded, "v1"},
		{"the zero weights", Weights{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.w.Version(); got != tc.want {
				t.Errorf("Version() = %q, want %q", got, tc.want)
			}
		})
	}
}

// initialShapeMean returns the mean of feature in the checked-in initial
// weights file, so the test compares against the file itself rather than a
// second copy of the number.
func initialShapeMean(t *testing.T, feature string) float64 {
	t.Helper()
	raw, err := os.ReadFile(initialWeightsPath)
	if err != nil {
		t.Fatalf("read initial weights: %v", err)
	}
	var f struct {
		Shape struct {
			Features []string  `json:"features"`
			Mean     []float64 `json:"mean"`
		} `json:"shape"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse initial weights: %v", err)
	}
	i := slices.Index(f.Shape.Features, feature)
	if i < 0 || i >= len(f.Shape.Mean) {
		t.Fatalf("initial weights have no mean for %q", feature)
	}
	return f.Shape.Mean[i]
}

// weightsWith returns the embedded weights file after mutate has changed its
// typed form, re-marshalled.
func weightsWith(t *testing.T, mutate func(*weightsFile)) []byte {
	t.Helper()
	var f weightsFile
	if err := json.Unmarshal(embeddedWeights, &f); err != nil {
		t.Fatalf("decode embedded weights: %v", err)
	}
	mutate(&f)
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("encode weights: %v", err)
	}
	return out
}

// replaceOnce returns the embedded weights file with the one occurrence of old
// replaced by new. It fails the test when old is not exactly once in the file,
// so a rejection row cannot pass against an unchanged file.
func replaceOnce(t *testing.T, old, new string) []byte {
	t.Helper()
	if n := bytes.Count(embeddedWeights, []byte(old)); n != 1 {
		t.Fatalf("%q occurs %d times in the embedded weights, want 1", old, n)
	}
	return bytes.Replace(embeddedWeights, []byte(old), []byte(new), 1)
}

func loadWeightsFrom(t *testing.T, data []byte) Weights {
	t.Helper()
	w, err := LoadWeights(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}
	return w
}
