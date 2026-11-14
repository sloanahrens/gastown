package storypoints

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// embeddedWeights is the checked-in weights file, a byte-for-byte copy of
// docs/plans/2026-11-11-story-points-initial-weights.json.
//
//go:embed weights.json
var embeddedWeights []byte

// defaultWeights is the embedded file, loaded once. A file that fails
// LoadWeights is a bug in the checked-in data, so this panics at init rather
// than handing every caller an error.
var defaultWeights = loadEmbedded()

// WeightsSource names the file a Weights value was read from.
type WeightsSource string

const (
	// SourceEmbedded is the weights.json compiled into the binary.
	SourceEmbedded WeightsSource = "embedded"
	// SourceRuntime is the town's adopted weights, the file LoadTownWeights
	// reads.
	SourceRuntime WeightsSource = "runtime"
)

// weightsVersion is the only layout of the weights file this code reads.
const weightsVersion = 1

// shapeFeatureNames lists the twelve shape features in the order the weights
// file and Shape.vector use.
var shapeFeatureNames = []string{
	"boxes", "paths", "pkgs", "dlen", "ncons", "nlab",
	"podeng", "tests", "nbt", "rig_gt", "rig_dv", "is_haiku",
}

// weightsFile is the wire form of weights.json. The fields this code does not
// use (trained_on, minutes_target, alpha) are not decoded.
type weightsFile struct {
	Version       int            `json:"version"`
	TrainedOn     string         `json:"trained_on"`
	MinutesTarget string         `json:"minutes_target"`
	Shape         shapeBlock     `json:"shape"`
	Knots         [][]float64    `json:"fits_twenty_minutes_to_minutes_knots"`
	Embedding     embeddingBlock `json:"embedding"`
}

type shapeBlock struct {
	Features  []string  `json:"features"`
	Mean      []float64 `json:"mean"`
	Scale     []float64 `json:"scale"`
	Coef      []float64 `json:"coef"`
	Intercept float64   `json:"intercept"`
	Alpha     float64   `json:"alpha"`
}

type embeddingBlock struct {
	Model     string    `json:"model"`
	Dim       int       `json:"dim"`
	Input     string    `json:"input"`
	Alpha     float64   `json:"alpha"`
	Coef      []float64 `json:"coef"`
	Intercept float64   `json:"intercept"`
}

// Weights is a validated weights file: the shape model, the fits-to-minutes
// table and the embedding ridge. Make one with LoadWeights, Default or
// LoadTownWeights; the zero value has no model, so ShapeMinutes returns
// ErrNoWeights. The slices are shared with the Default value, so callers must
// not modify them.
type Weights struct {
	version int
	source  WeightsSource

	shapeMean      []float64
	shapeScale     []float64
	shapeCoef      []float64
	shapeIntercept float64

	knots []knot

	embedModel     string
	embedInput     string
	embedCoef      []float64
	embedIntercept float64
}

// knot is one row of the fits_twenty_minutes table: a probability and the
// minutes it maps to. Later tasks interpolate between knots.
type knot struct {
	fits    float64
	minutes float64
}

// Default returns the embedded weights.
func Default() Weights {
	return defaultWeights
}

func loadEmbedded() Weights {
	w, err := LoadWeights(bytes.NewReader(embeddedWeights))
	if err != nil {
		panic("storypoints: embedded weights.json: " + err.Error())
	}
	w.source = SourceEmbedded
	return w
}

// Version names the model these weights are: where they were read from and the
// weights file's version, such as "embedded v1" or "runtime v1". Weights built
// by LoadWeights from bytes the caller supplied name the file version alone,
// and the zero Weights names nothing.
func (w Weights) Version() string {
	if w.version == 0 {
		return ""
	}
	if w.source == "" {
		return "v" + strconv.Itoa(w.version)
	}
	return string(w.source) + " v" + strconv.Itoa(w.version)
}

// RuntimeWeightsPath is the file a town adopts a retrained model into:
// <townRoot>/.runtime/storypoints/weights.json. An empty town root names no
// file, so a loader given one reads no runtime weights.
func RuntimeWeightsPath(townRoot string) string {
	if townRoot == "" {
		return ""
	}
	return filepath.Join(townRoot, ".runtime", "storypoints", "weights.json")
}

// LoadTownWeights returns the weights to estimate with in townRoot: the runtime
// file when it is present and loads, the embedded weights otherwise. A runtime
// file that cannot be read or does not load is reported in one logged line and
// is never an error, so an estimate always has weights; a missing file is
// silent, because a town that has adopted no model is the normal case.
func LoadTownWeights(townRoot string) Weights {
	return loadTownWeights(townRoot, os.ReadFile, log.Printf)
}

// loadTownWeights is LoadTownWeights with the file read and the logger passed
// in, so a test drives every branch without a filesystem or a real log.
func loadTownWeights(townRoot string, read func(string) ([]byte, error), logf func(string, ...any)) Weights {
	path := RuntimeWeightsPath(townRoot)
	if path == "" {
		return defaultWeights
	}
	data, err := read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaultWeights
		}
		return reportRuntimeWeights(logf, path, err)
	}
	w, err := LoadWeights(bytes.NewReader(data))
	if err != nil {
		return reportRuntimeWeights(logf, path, err)
	}
	w.source = SourceRuntime
	return w
}

// reportRuntimeWeights logs the one line a rejected runtime file gets and
// returns the embedded weights the estimate goes on with.
func reportRuntimeWeights(logf func(string, ...any), path string, err error) Weights {
	logf("storypoints: runtime weights %s not used, the embedded weights are in use: %v", path, err)
	return defaultWeights
}

// LoadWeights decodes and validates one weights file. It refuses a file whose
// shape features differ from shapeFeatureNames, whose shape lists do not have
// one entry per feature, whose scale is not positive, whose knot table is
// malformed or unsorted, whose embedding block is inconsistent, or that has
// data after the JSON object. encoding/json cannot represent NaN or Inf, so a
// NaN or out-of-range number is refused by the decoder.
func LoadWeights(r io.Reader) (Weights, error) {
	dec := json.NewDecoder(r)
	var f weightsFile
	if err := dec.Decode(&f); err != nil {
		return Weights{}, fmt.Errorf("storypoints: decode weights: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Weights{}, errors.New("storypoints: weights: data after the JSON object")
	}
	return f.check()
}

// check validates the decoded file and copies it into a Weights.
func (f weightsFile) check() (Weights, error) {
	if f.Version != weightsVersion {
		return Weights{}, fmt.Errorf("storypoints: weights version %d, want %d", f.Version, weightsVersion)
	}
	if !slices.Equal(f.Shape.Features, shapeFeatureNames) {
		return Weights{}, fmt.Errorf("storypoints: shape features %q differ from the code's %q",
			f.Shape.Features, shapeFeatureNames)
	}
	n := len(shapeFeatureNames)
	for _, list := range []struct {
		name string
		vals []float64
	}{{"mean", f.Shape.Mean}, {"scale", f.Shape.Scale}, {"coef", f.Shape.Coef}} {
		if len(list.vals) != n {
			return Weights{}, fmt.Errorf("storypoints: shape %s has %d entries, want %d", list.name, len(list.vals), n)
		}
	}
	for i, s := range f.Shape.Scale {
		if !(s > 0) {
			return Weights{}, fmt.Errorf("storypoints: shape scale of %s is %v, want a positive number",
				shapeFeatureNames[i], s)
		}
	}

	if f.Embedding.Model == "" {
		return Weights{}, errors.New("storypoints: embedding has no model name")
	}
	if f.Embedding.Dim <= 0 || len(f.Embedding.Coef) != f.Embedding.Dim {
		return Weights{}, fmt.Errorf("storypoints: embedding has %d coefficients, want its dim %d",
			len(f.Embedding.Coef), f.Embedding.Dim)
	}

	if len(f.Knots) == 0 {
		return Weights{}, errors.New("storypoints: fits_twenty_minutes table is empty")
	}
	knots := make([]knot, 0, len(f.Knots))
	for i, row := range f.Knots {
		if len(row) != 2 {
			return Weights{}, fmt.Errorf("storypoints: fits_twenty_minutes row %d has %d numbers, want 2", i, len(row))
		}
		k := knot{fits: row[0], minutes: row[1]}
		if k.fits < 0 || k.fits > 1 {
			return Weights{}, fmt.Errorf("storypoints: fits_twenty_minutes row %d probability %v is outside 0 to 1", i, k.fits)
		}
		if !(k.minutes > 0) {
			return Weights{}, fmt.Errorf("storypoints: fits_twenty_minutes row %d minutes %v, want a positive number", i, k.minutes)
		}
		if i > 0 && k.fits < knots[i-1].fits {
			return Weights{}, fmt.Errorf("storypoints: fits_twenty_minutes row %d is out of order", i)
		}
		knots = append(knots, k)
	}

	return Weights{
		version:        f.Version,
		shapeMean:      f.Shape.Mean,
		shapeScale:     f.Shape.Scale,
		shapeCoef:      f.Shape.Coef,
		shapeIntercept: f.Shape.Intercept,
		knots:          knots,
		embedModel:     f.Embedding.Model,
		embedInput:     f.Embedding.Input,
		embedCoef:      f.Embedding.Coef,
		embedIntercept: f.Embedding.Intercept,
	}, nil
}

// ErrNoWeights reports a Weights with no shape model, such as the zero value.
var ErrNoWeights = errors.New("storypoints: weights are not loaded")

// ShapeMinutes is the shape model's predicted polecat minutes for s:
// exp(intercept + sum of coef_i * (x_i - mean_i) / scale_i). It returns
// ErrNoWeights when w has no shape model.
func (w Weights) ShapeMinutes(s Shape) (float64, error) {
	n := len(shapeFeatureNames)
	if len(w.shapeMean) != n || len(w.shapeScale) != n || len(w.shapeCoef) != n {
		return 0, ErrNoWeights
	}
	return math.Exp(w.shapeLog(s)), nil
}

// shapeLog is the shape model's log minutes for s. It reads the shape lists as
// LoadWeights checked them, so it has no error to return; ShapeMinutes checks
// that a Weights has them at all.
func (w Weights) shapeLog(s Shape) float64 {
	z := w.shapeIntercept
	for i, x := range s.vector() {
		z += w.shapeCoef[i] * (x - w.shapeMean[i]) / w.shapeScale[i]
	}
	return z
}

// ShapeMean returns the training mean of the named shape feature. ok is false
// when the name is not a shape feature.
func (w Weights) ShapeMean(feature string) (mean float64, ok bool) {
	i := slices.Index(shapeFeatureNames, feature)
	if i < 0 {
		return 0, false
	}
	return w.shapeMean[i], true
}

// EmbeddingModel is the name of the embedding model the embedding ridge was fitted
// on, "" when w has no embedding ridge.
func (w Weights) EmbeddingModel() string {
	return w.embedModel
}

// vector returns the features in shapeFeatureNames order.
func (s Shape) vector() []float64 {
	return []float64{
		s.Boxes, s.Paths, s.Pkgs, s.DLen, s.NCons, s.NLab,
		s.PodEng, s.Tests, s.NBt, s.RigGT, s.RigDV, s.IsHaiku,
	}
}
