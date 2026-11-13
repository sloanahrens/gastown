package storypoints

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// testBaseJSON is a checked-in-shaped weights file of the test's embedding
// dimension: flat shape coefficients (every bead predicts one minute), knots that
// map a probability to minutes, and an embedding ridge of zeros.
func testBaseJSON(dim int) string {
	zeros := func(n int) string { return strings.TrimSuffix(strings.Repeat("0,", n), ",") }
	ones := func(n int) string { return strings.TrimSuffix(strings.Repeat("1,", n), ",") }
	features := `"boxes","paths","pkgs","dlen","ncons","nlab","podeng","tests","nbt","rig_gt","rig_dv","is_haiku"`
	return fmt.Sprintf(`{"version":1,
	"shape":{"features":[%s],"mean":[%s],"scale":[%s],"coef":[%s],"intercept":0,"alpha":1},
	"fits_twenty_minutes_to_minutes_knots":[[0,1],[1,100]],
	"embedding":{"model":"test-embed","dim":%d,"input":"test input","alpha":1,"intercept":0,"coef":[%s]}}`,
		features, zeros(12), ones(12), zeros(12), dim, zeros(dim))
}

// testBase is the checked-in-shaped weights of dimension dim.
func testBase(t *testing.T, dim int) Weights {
	t.Helper()
	w, err := LoadWeights(strings.NewReader(testBaseJSON(dim)))
	if err != nil {
		t.Fatalf("LoadWeights(test base): %v", err)
	}
	return w
}

// syntheticSamples draws n landed beads in landing order. The log minutes are a
// linear function of the shape features, and each embedding component is a linear
// function of the same features, so the shape ridge and the embedding ridge both
// contain the signal and their blend recovers it. The draw is seeded, so a case
// reads the same samples each run.
func syntheticSamples(n int, seed uint64, dim int) []Sample {
	rng := rand.New(rand.NewPCG(seed, seed))
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Sample, n)
	for i := range out {
		s := Shape{
			Boxes:   float64(rng.IntN(8)),
			Paths:   rng.Float64() * 6,
			Pkgs:    rng.Float64() * 3,
			DLen:    4 + rng.Float64()*4,
			NCons:   float64(rng.IntN(6)),
			NLab:    float64(rng.IntN(4)),
			PodEng:  0,
			Tests:   float64(rng.IntN(10)),
			NBt:     float64(rng.IntN(30)),
			RigGT:   float64(rng.IntN(2)),
			RigDV:   float64(rng.IntN(2)),
			IsHaiku: float64(rng.IntN(2)),
		}
		derived := []float64{s.Boxes / 8, (s.DLen - 6) / 2, s.IsHaiku - 0.5, s.NBt / 30}
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = float32(derived[j%len(derived)])
		}
		logMinutes := 1.2 + 0.15*s.Boxes + 0.3*(s.DLen-6) - 0.4*s.IsHaiku
		out[i] = Sample{
			Shape:    s,
			Vector:   vec,
			Minutes:  math.Exp(logMinutes),
			LandedAt: start.Add(time.Duration(i) * time.Hour),
		}
	}
	return out
}

// TestRetrainRefusesTooFewSamples: a fit on fewer beads than the folds and the
// holdout can read is refused, with no model returned.
func TestRetrainRefusesTooFewSamples(t *testing.T) {
	t.Parallel()
	base := testBase(t, 3)
	_, err := Retrain(base, syntheticSamples(MinRetrainSamples-1, 1, 3))
	if err == nil {
		t.Fatalf("Retrain over %d samples returned no error", MinRetrainSamples-1)
	}
}

// TestRetrainRefusesBadSamples: a sample that cannot be fitted stops the retrain
// rather than being dropped, so the fit never reads a bead it was not given.
func TestRetrainRefusesBadSamples(t *testing.T) {
	t.Parallel()
	base := testBase(t, 3)
	cases := []struct {
		name   string
		mutate func(s *Sample)
	}{
		{"zero minutes", func(s *Sample) { s.Minutes = 0 }},
		{"NaN minutes", func(s *Sample) { s.Minutes = math.NaN() }},
		{"infinite minutes", func(s *Sample) { s.Minutes = math.Inf(1) }},
		{"embedding of another size", func(s *Sample) { s.Vector = s.Vector[:2] }},
		{"no landing time", func(s *Sample) { s.LandedAt = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			samples := syntheticSamples(MinRetrainSamples+10, 2, 3)
			tc.mutate(&samples[5])
			if _, err := Retrain(base, samples); err == nil {
				t.Fatalf("Retrain returned no error for a %s sample", tc.name)
			}
		})
	}
}

// TestRetrainBeatsCheckedInOnHoldout: on the latest fifth of the samples the
// proposed weights are closer to the measured minutes than the checked-in weights,
// which predict one minute for every bead.
func TestRetrainBeatsCheckedInOnHoldout(t *testing.T) {
	t.Parallel()
	base := testBase(t, 4)
	m, err := Retrain(base, syntheticSamples(120, 3, 4))
	if err != nil {
		t.Fatalf("Retrain: %v", err)
	}
	if want := 120 / 5; len(m.Holdout) != want {
		t.Fatalf("holdout has %d samples, want %d", len(m.Holdout), want)
	}
	var proposedErr, checkedErr float64
	for _, h := range m.Holdout {
		proposedErr += math.Abs(math.Log(h.Proposed) - math.Log(h.Actual))
		checkedErr += math.Abs(math.Log(h.Checked) - math.Log(h.Actual))
	}
	if mean := proposedErr / float64(len(m.Holdout)); mean > 0.1 {
		t.Errorf("proposed mean log error on the holdout = %.3f, want the signal recovered (under 0.1)", mean)
	}
	if !(proposedErr < checkedErr/10) {
		t.Errorf("proposed log error %.3f, checked-in %.3f: the fit does not beat the checked-in weights", proposedErr, checkedErr)
	}
}

// TestRetrainEncodesWhatLoadWeightsReads: the file a retrain writes is one LoadWeights
// accepts, and it predicts what the model it was written from predicts.
func TestRetrainEncodesWhatLoadWeightsReads(t *testing.T) {
	t.Parallel()
	base := testBase(t, 3)
	samples := syntheticSamples(90, 4, 3)
	m, err := Retrain(base, samples)
	if err != nil {
		t.Fatalf("Retrain: %v", err)
	}
	var buf bytes.Buffer
	if err := m.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	read, err := LoadWeights(&buf)
	if err != nil {
		t.Fatalf("LoadWeights(encoded): %v", err)
	}
	s := samples[7]
	want, err := m.Weights.ShapeMinutes(s.Shape)
	if err != nil {
		t.Fatalf("model ShapeMinutes: %v", err)
	}
	got, err := read.ShapeMinutes(s.Shape)
	if err != nil {
		t.Fatalf("read ShapeMinutes: %v", err)
	}
	if math.Abs(got-want) > 1e-9*want {
		t.Errorf("read shape minutes = %v, want %v", got, want)
	}
	wantEmbed, err := m.Weights.EmbeddingMinutes(s.Vector)
	if err != nil {
		t.Fatalf("model EmbeddingMinutes: %v", err)
	}
	gotEmbed, err := read.EmbeddingMinutes(s.Vector)
	if err != nil {
		t.Fatalf("read EmbeddingMinutes: %v", err)
	}
	if math.Abs(gotEmbed-wantEmbed) > 1e-9*wantEmbed {
		t.Errorf("read embedding minutes = %v, want %v", gotEmbed, wantEmbed)
	}
	if read.EmbeddingModel() != "test-embed" {
		t.Errorf("embedding model = %q, want the checked-in model", read.EmbeddingModel())
	}
}

// TestRetrainHoldoutScoresTheWrittenFile: the holdout's proposed predictions are the
// predictions of the file a retrain writes, read back by LoadWeights. A file fitted on
// more beads than the holdout was scored on would fail this.
func TestRetrainHoldoutScoresTheWrittenFile(t *testing.T) {
	t.Parallel()
	base := testBase(t, 3)
	samples := syntheticSamples(120, 5, 3)
	m, err := Retrain(base, samples)
	if err != nil {
		t.Fatalf("Retrain: %v", err)
	}
	var buf bytes.Buffer
	if err := m.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	read, err := LoadWeights(&buf)
	if err != nil {
		t.Fatalf("LoadWeights(encoded): %v", err)
	}
	// The samples are in landing order and one hour apart, so the holdout is the last
	// fifth of them: the 24 beads after the first 96.
	hold := samples[len(samples)-len(m.Holdout):]
	if len(m.Holdout) != 24 {
		t.Fatalf("holdout has %d samples, want 24", len(m.Holdout))
	}
	for i, h := range m.Holdout {
		s := hold[i]
		shape, err := read.ShapeMinutes(s.Shape)
		if err != nil {
			t.Fatalf("read ShapeMinutes: %v", err)
		}
		embed, err := read.EmbeddingMinutes(s.Vector)
		if err != nil {
			t.Fatalf("read EmbeddingMinutes: %v", err)
		}
		if got := math.Sqrt(shape * embed); math.Abs(got-h.Proposed) > 1e-9*h.Proposed {
			t.Errorf("holdout bead %d: the written file predicts %v, the holdout scored %v", i, got, h.Proposed)
		}
	}
}

// TestRetrainRefusesWhatItCannotFit: a base with no model, a sample whose values are
// not numbers the ridge can solve, and samples that all landed at one time stop the
// retrain with an error rather than a model.
func TestRetrainRefusesWhatItCannotFit(t *testing.T) {
	t.Parallel()
	base := testBase(t, 3)
	cases := []struct {
		name    string
		base    Weights
		mutate  func(samples []Sample)
		wantErr error
	}{
		{name: "base with no model", base: Weights{}, wantErr: ErrNoWeights},
		{name: "NaN shape feature", base: base, mutate: func(s []Sample) { s[5].Shape.Boxes = math.NaN() }},
		{name: "NaN embedding component", base: base, mutate: func(s []Sample) { s[5].Vector[1] = float32(math.NaN()) }},
		// The fit's last quarter is only scored by the penalty search, so a NaN there
		// passes the search and fails the fit over all of the fit's samples.
		{name: "NaN feature in the fit's last quarter", base: base, mutate: func(s []Sample) { s[50].Shape.Boxes = math.NaN() }},
		{name: "NaN feature in the holdout", base: base, mutate: func(s []Sample) { s[60].Shape.Boxes = math.NaN() }},
		{name: "every bead at one landing time", base: base, mutate: func(s []Sample) {
			for i := range s {
				s[i].LandedAt = s[0].LandedAt
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			samples := syntheticSamples(MinRetrainSamples+10, 6, 3)
			if tc.mutate != nil {
				tc.mutate(samples)
			}
			_, err := Retrain(tc.base, samples)
			if err == nil {
				t.Fatalf("Retrain returned no error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestLearnersRefuseNoRowsAndNoPenalty: the learners and the joint fit refuse the
// inputs their solves cannot take: no rows, or a penalty that is not positive.
func TestLearnersRefuseNoRowsAndNoPenalty(t *testing.T) {
	t.Parallel()
	rows := syntheticSamples(MinRetrainSamples, 7, 3)
	if _, err := shapeLearner(nil); err == nil {
		t.Errorf("shapeLearner(no rows) returned no error")
	}
	if _, err := embedLearner(nil); err == nil {
		t.Errorf("embedLearner(no rows) returned no error")
	}
	shapeAt, err := shapeLearner(rows)
	if err != nil {
		t.Fatalf("shapeLearner: %v", err)
	}
	if _, err := shapeAt(0); err == nil {
		t.Errorf("shape fit at penalty 0 returned no error")
	}
	embedAt, err := embedLearner(rows)
	if err != nil {
		t.Fatalf("embedLearner: %v", err)
	}
	if _, err := embedAt(0); err == nil {
		t.Errorf("embedding fit at penalty 0 returned no error")
	}
	if _, err := fitBoth(rows, 0, 1); err == nil {
		t.Errorf("fitBoth with shape penalty 0 returned no error")
	}
	if _, err := fitBoth(rows, 1, 0); err == nil {
		t.Errorf("fitBoth with embedding penalty 0 returned no error")
	}
	if _, err := fitBoth(nil, 1, 1); err == nil {
		t.Errorf("fitBoth over no rows returned no error")
	}
	noVectors := syntheticSamples(MinRetrainSamples, 7, 0)
	if _, err := fitBoth(noVectors, 1, 1); err == nil {
		t.Errorf("fitBoth over embeddings with no numbers returned no error")
	}
}

// TestChooseAlphaReportsALearnerFailure: a learner that cannot fit, or a fit that
// cannot predict, stops the choice of penalty with its error.
func TestChooseAlphaReportsALearnerFailure(t *testing.T) {
	t.Parallel()
	rows := syntheticSamples(MinRetrainSamples, 8, 3)
	boom := errors.New("boom")
	failFit := func([]Sample) (func(float64) (predictor, error), error) { return nil, boom }
	failPredict := func([]Sample) (func(float64) (predictor, error), error) {
		return func(float64) (predictor, error) { return nil, boom }, nil
	}
	for name, learn := range map[string]learner{"fit": failFit, "predict": failPredict} {
		if _, err := chooseAlpha(rows, learn); !errors.Is(err, boom) {
			t.Errorf("%s failure: err = %v, want the learner's error", name, err)
		}
	}
}

// biasedPredictor predicts the measured log minutes plus a fixed bias, so a
// penalty's fit scores by how far its bias is from zero.
type biasedPredictor struct{ bias float64 }

func (p biasedPredictor) logMinutes(s Sample) float64 { return math.Log(s.Minutes) + p.bias }

// TestChooseAlphaPicksTheLeastErrorPenalty: the penalty whose fits score lowest is
// chosen, wherever it sits in the list.
func TestChooseAlphaPicksTheLeastErrorPenalty(t *testing.T) {
	t.Parallel()
	rows := syntheticSamples(MinRetrainSamples, 9, 3)
	learn := func([]Sample) (func(float64) (predictor, error), error) {
		return func(alpha float64) (predictor, error) {
			return biasedPredictor{bias: math.Abs(math.Log(alpha / 10))}, nil
		}, nil
	}
	got, err := chooseAlpha(rows, learn)
	if err != nil {
		t.Fatalf("chooseAlpha: %v", err)
	}
	if got != 10 {
		t.Errorf("chooseAlpha = %v, want 10 (the penalty with no bias)", got)
	}
}
