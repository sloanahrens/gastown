package storypoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"
)

// MinRetrainSamples is the fewest landed beads a retrain fits. Fewer than this
// and the time-ordered folds and the holdout are too small to read.
const MinRetrainSamples = 60

// holdoutShare is the share of the samples, the latest in landing order, that the
// holdout keeps and the fit does not see.
const holdoutShare = 0.2

// retrainFolds is how many time-ordered folds choose each penalty. Fold f fits on
// the blocks before block f and scores block f, so no fold trains on the future.
const retrainFolds = 3

// retrainAlphas is the fixed list of penalties a retrain chooses from. The
// checked-in penalties, 162.4 for the shape ridge and 1.78 for the embedding ridge,
// both fall between its ends.
var retrainAlphas = []float64{0.1, 1, 10, 100, 1000}

// Sample is one landed bead the retrain fits: its shape features, the embedding
// of its section text, its measured polecat minutes and when it landed.
type Sample struct {
	Shape    Shape
	Vector   []float32
	Minutes  float64
	LandedAt time.Time
}

// Holdout is one holdout sample: its measured minutes, and the minutes the
// proposed weights and the checked-in weights predict for it. Both predictions
// are the blend of the shape and embedding models.
type Holdout struct {
	Actual   float64
	Proposed float64
	Checked  float64
}

// Model is one retrain: the proposed weights, the penalties they were fitted at,
// the span of time they were fitted over, and the holdout the proposed weights
// are scored on.
type Model struct {
	Weights    Weights
	ShapeAlpha float64
	EmbedAlpha float64
	TrainedOn  string
	Trained    int
	Holdout    []Holdout
}

// predictor is a fitted model: the natural log of the minutes it predicts.
type predictor interface {
	logMinutes(s Sample) float64
}

// learner fits a model over rows, returning a function that fits at one penalty.
type learner func(rows []Sample) (func(alpha float64) (predictor, error), error)

// shapeFit is the shape ridge in the weights file's layout: coefficients on the
// standardized features.
type shapeFit struct {
	mean, scale, coef []float64
	intercept         float64
}

func (f shapeFit) logMinutes(s Sample) float64 {
	z := f.intercept
	for i, x := range s.Shape.vector() {
		z += f.coef[i] * (x - f.mean[i]) / f.scale[i]
	}
	return z
}

// embedFit is the embedding ridge: coefficients on the raw vector.
type embedFit struct {
	coef      []float64
	intercept float64
}

func (f embedFit) logMinutes(s Sample) float64 {
	z := f.intercept
	for i, x := range s.Vector {
		z += f.coef[i] * float64(x)
	}
	return z
}

// shapeLearner standardizes the shape features over rows, the way the weights file
// reads them, and fits the ridge on the standardized values.
func shapeLearner(rows []Sample) (func(float64) (predictor, error), error) {
	d := len(shapeFeatureNames)
	n := float64(len(rows))
	mean := make([]float64, d)
	scale := make([]float64, d)
	for _, s := range rows {
		for j, x := range s.Shape.vector() {
			mean[j] += x / n
		}
	}
	for _, s := range rows {
		for j, x := range s.Shape.vector() {
			scale[j] += (x - mean[j]) * (x - mean[j]) / n
		}
	}
	for j := range scale {
		scale[j] = math.Sqrt(scale[j])
		if !(scale[j] > 0) {
			// A feature with no spread in the fit standardizes to zero, so it
			// cannot carry a coefficient: its scale is 1 and its standardized
			// values are all zero, so its coefficient is zero.
			scale[j] = 1
		}
	}
	xs := make([][]float64, len(rows))
	ys := make([]float64, len(rows))
	for i, s := range rows {
		z := make([]float64, d)
		for j, x := range s.Shape.vector() {
			z[j] = (x - mean[j]) / scale[j]
		}
		xs[i] = z
		ys[i] = math.Log(s.Minutes)
	}
	r, err := newRidge(xs, ys)
	if err != nil {
		return nil, err
	}
	return func(alpha float64) (predictor, error) {
		coef, intercept, err := r.solve(alpha)
		if err != nil {
			return nil, err
		}
		return shapeFit{mean: mean, scale: scale, coef: coef, intercept: intercept}, nil
	}, nil
}

// embedLearner fits the embedding ridge on the raw vectors.
func embedLearner(rows []Sample) (func(float64) (predictor, error), error) {
	xs := make([][]float64, len(rows))
	ys := make([]float64, len(rows))
	for i, s := range rows {
		v := make([]float64, len(s.Vector))
		for j, x := range s.Vector {
			v[j] = float64(x)
		}
		xs[i] = v
		ys[i] = math.Log(s.Minutes)
	}
	r, err := newRidge(xs, ys)
	if err != nil {
		return nil, err
	}
	return func(alpha float64) (predictor, error) {
		coef, intercept, err := r.solve(alpha)
		if err != nil {
			return nil, err
		}
		return embedFit{coef: coef, intercept: intercept}, nil
	}, nil
}

// chooseAlpha returns the penalty in retrainAlphas with the least squared error
// of log minutes summed over the time-ordered folds. The first of equal scores
// wins, so a tie takes the smaller penalty.
func chooseAlpha(rows []Sample, learn learner) (float64, error) {
	bounds := make([]int, retrainFolds+2)
	for k := range bounds {
		bounds[k] = len(rows) * k / (retrainFolds + 1)
	}
	score := make([]float64, len(retrainAlphas))
	for f := 1; f <= retrainFolds; f++ {
		fitAt, err := learn(rows[:bounds[f]])
		if err != nil {
			return 0, err
		}
		valid := rows[bounds[f]:bounds[f+1]]
		for i, alpha := range retrainAlphas {
			p, err := fitAt(alpha)
			if err != nil {
				return 0, err
			}
			for _, s := range valid {
				e := p.logMinutes(s) - math.Log(s.Minutes)
				score[i] += e * e
			}
		}
	}
	best := 0
	for i := range score {
		if score[i] < score[best] {
			best = i
		}
	}
	return retrainAlphas[best], nil
}

// Retrain fits the proposed weights on samples. base supplies what a retrain keeps
// from the checked-in file: the fits-to-minutes knots, the embedding model and its
// input text, and the checked-in predictions the holdout compares against.
//
// The penalties are chosen on the earlier 80% of the samples in landing order. The
// proposed weights are fitted on that same earlier 80% at the chosen penalties, and
// the holdout, the latest 20%, is scored on those weights. The weights Retrain
// returns are the ones that were scored: they are not refitted on every sample,
// because that would put the holdout's beads into a model nobody measured. A person
// who wants a model on all the beads retrains later. The holdout boundary moves
// forward past a landing time shared with the sample before it, so one time never
// falls in both.
//
// The fits-to-minutes knots are the checked-in model's and are not refit here: they
// are copied into the proposed weights unchanged, so recalibrate them before the
// file is adopted.
func Retrain(base Weights, samples []Sample) (Model, error) {
	if base.EmbeddingDimension() == 0 || len(base.shapeCoef) != len(shapeFeatureNames) {
		return Model{}, ErrNoWeights
	}
	if len(samples) < MinRetrainSamples {
		return Model{}, fmt.Errorf("storypoints: retrain has %d samples, want at least %d", len(samples), MinRetrainSamples)
	}
	dim := base.EmbeddingDimension()
	for i, s := range samples {
		if !(s.Minutes > 0) || math.IsInf(s.Minutes, 0) {
			return Model{}, fmt.Errorf("storypoints: retrain sample %d has %v minutes, want a positive number", i, s.Minutes)
		}
		if len(s.Vector) != dim {
			return Model{}, fmt.Errorf("storypoints: retrain sample %d has a %d-number embedding, want %d", i, len(s.Vector), dim)
		}
		// A value that is not a number would give the holdout a prediction that is not
		// one, so it stops the retrain here, whichever side of the split it falls on.
		for _, x := range s.Shape.vector() {
			if !isFinite(x) {
				return Model{}, fmt.Errorf("storypoints: retrain sample %d has a shape feature that is not a finite number", i)
			}
		}
		for _, x := range s.Vector {
			if !isFinite(float64(x)) {
				return Model{}, fmt.Errorf("storypoints: retrain sample %d has an embedding value that is not a finite number", i)
			}
		}
		if s.LandedAt.IsZero() {
			return Model{}, fmt.Errorf("storypoints: retrain sample %d has no landing time", i)
		}
	}
	sorted := slices.Clone(samples)
	slices.SortStableFunc(sorted, func(a, b Sample) int { return a.LandedAt.Compare(b.LandedAt) })

	cut := int(math.Round(float64(len(sorted)) * (1 - holdoutShare)))
	for cut < len(sorted) && sorted[cut].LandedAt.Equal(sorted[cut-1].LandedAt) {
		cut++
	}
	if cut < retrainFolds+1 || cut == len(sorted) {
		return Model{}, errors.New("storypoints: retrain cannot split the samples into a fit and a holdout")
	}
	fit, hold := sorted[:cut], sorted[cut:]

	shapeAlpha, err := chooseAlpha(fit, shapeLearner)
	if err != nil {
		return Model{}, fmt.Errorf("storypoints: shape penalty: %w", err)
	}
	embedAlpha, err := chooseAlpha(fit, embedLearner)
	if err != nil {
		return Model{}, fmt.Errorf("storypoints: embedding penalty: %w", err)
	}

	m := Model{
		ShapeAlpha: shapeAlpha,
		EmbedAlpha: embedAlpha,
		Trained:    len(fit),
		TrainedOn: fmt.Sprintf("landed beads %s..%s, n=%d",
			fit[0].LandedAt.Format(time.DateOnly), fit[len(fit)-1].LandedAt.Format(time.DateOnly), len(fit)),
	}

	holdFit, err := fitBoth(fit, shapeAlpha, embedAlpha)
	if err != nil {
		return Model{}, err
	}
	for _, s := range hold {
		proposed := math.Exp((holdFit.shape.logMinutes(s) + holdFit.embed.logMinutes(s)) / 2)
		m.Holdout = append(m.Holdout, Holdout{
			Actual:   s.Minutes,
			Proposed: proposed,
			Checked:  math.Exp((base.shapeLog(s.Shape) + base.embedLog(s.Vector)) / 2),
		})
	}

	m.Weights = Weights{
		shapeMean:      holdFit.shape.mean,
		shapeScale:     holdFit.shape.scale,
		shapeCoef:      holdFit.shape.coef,
		shapeIntercept: holdFit.shape.intercept,
		knots:          base.knots,
		embedModel:     base.embedModel,
		embedCoef:      holdFit.embed.coef,
		embedIntercept: holdFit.embed.intercept,
		embedInput:     base.embedInput,
	}
	return m, nil
}

// fitted is the shape and embedding fits at one set of penalties.
type fitted struct {
	shape shapeFit
	embed embedFit
}

// fitBoth fits both ridges over rows at the given penalties.
func fitBoth(rows []Sample, shapeAlpha, embedAlpha float64) (fitted, error) {
	shapeAt, err := shapeLearner(rows)
	if err != nil {
		return fitted{}, fmt.Errorf("storypoints: shape fit: %w", err)
	}
	s, err := shapeAt(shapeAlpha)
	if err != nil {
		return fitted{}, fmt.Errorf("storypoints: shape fit: %w", err)
	}
	embedAt, err := embedLearner(rows)
	if err != nil {
		return fitted{}, fmt.Errorf("storypoints: embedding fit: %w", err)
	}
	e, err := embedAt(embedAlpha)
	if err != nil {
		return fitted{}, fmt.Errorf("storypoints: embedding fit: %w", err)
	}
	return fitted{shape: s.(shapeFit), embed: e.(embedFit)}, nil
}

// Encode writes the model in the weights file's layout, which LoadWeights reads
// back. It carries the training span and the penalties, which LoadWeights does not
// read, so a person adopting the file can see how it was fitted. The knots are the
// checked-in model's, copied unchanged (see Retrain).
func (m Model) Encode(w io.Writer) error {
	knots := make([][]float64, len(m.Weights.knots))
	for i, k := range m.Weights.knots {
		knots[i] = []float64{k.fits, k.minutes}
	}
	f := weightsFile{
		Version:       weightsVersion,
		TrainedOn:     m.TrainedOn,
		MinutesTarget: "log of polecat minutes",
		Shape: shapeBlock{
			Features:  shapeFeatureNames,
			Mean:      m.Weights.shapeMean,
			Scale:     m.Weights.shapeScale,
			Coef:      m.Weights.shapeCoef,
			Intercept: m.Weights.shapeIntercept,
			Alpha:     m.ShapeAlpha,
		},
		Knots: knots,
		Embedding: embeddingBlock{
			Model:     m.Weights.embedModel,
			Dim:       len(m.Weights.embedCoef),
			Input:     m.Weights.embedInput,
			Alpha:     m.EmbedAlpha,
			Intercept: m.Weights.embedIntercept,
			Coef:      m.Weights.embedCoef,
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}
