package metrics

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

var autoRevertAdoptedAt = time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC)

// autoRevertSamples are n samples that landed after the adoption, with boxes 1..n, each of
// minutes(i, prediction of the adopted weights, prediction of the previous weights) and the
// given first-round flag.
func autoRevertSamples(t *testing.T, n int, firstRound bool, adopted, previous storypoints.Weights, minutes func(i int, pAdopted, pPrevious float64) float64) []storypoints.Sample {
	t.Helper()
	var out []storypoints.Sample
	for i := 1; i <= n; i++ {
		s := storypoints.Sample{
			Shape:      storypoints.Shape{Boxes: float64(i)},
			Vector:     make([]float32, trainTestDim),
			LandedAt:   autoRevertAdoptedAt.Add(time.Duration(i) * time.Hour),
			FirstRound: firstRound,
		}
		pa, err := storyPointsBlend(adopted, s)
		if err != nil {
			t.Fatal(err)
		}
		pp, err := storyPointsBlend(previous, s)
		if err != nil {
			t.Fatal(err)
		}
		s.Minutes = minutes(i, pa, pp)
		out = append(out, s)
	}
	return out
}

// TestStoryPointsAutoRevertJudges: the rule of the week-after check reverts the adopted weights
// when their first-round rmse of the log ratio is higher than the replaced weights' by more than
// the margin, keeps them otherwise, and says so with both rmses (gt-q2rrvb).
func TestStoryPointsAutoRevertJudges(t *testing.T) {
	t.Parallel()
	adopted := adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 2.9})
	previous := adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 3.5})
	// R is the rms log distance between the two weights' predictions on the samples.
	probe := autoRevertSamples(t, 12, true, adopted, previous, func(int, float64, float64) float64 { return 1 })
	var sq float64
	for _, s := range probe {
		pa, _ := storyPointsBlend(adopted, s)
		pp, _ := storyPointsBlend(previous, s)
		sq += math.Pow(math.Log(pp/pa), 2)
	}
	r := math.Sqrt(sq / float64(len(probe)))
	if r < 0.05 {
		t.Fatalf("fixture: the two weights differ by %.4f rms, want at least 0.05", r)
	}
	// between returns minutes whose log sits a fraction lambda of the way from the adopted
	// prediction to the previous one: the adopted rmse is lambda*R and the previous (1-lambda)*R.
	between := func(lambda float64) func(int, float64, float64) float64 {
		return func(_ int, pa, pp float64) float64 { return math.Exp((1-lambda)*math.Log(pa) + lambda*math.Log(pp)) }
	}

	for _, tc := range []struct {
		name       string
		lambda     float64
		wantRevert bool
	}{
		{"actuals on the previous weights revert the adopted ones", 1, true},
		{"actuals on the adopted weights keep them", 0, false},
		{"just over the margin reverts", (1 + 0.031/r) / 2, true},
		{"just under the margin keeps", (1 + 0.029/r) / 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := judgeStoryPointsAdoption(autoRevertSamples(t, 12, true, adopted, previous, between(tc.lambda)), adopted, previous, autoRevertAdoptedAt)
			if err != nil {
				t.Fatalf("judge: %v", err)
			}
			if v.Revert != tc.wantRevert || v.Waits {
				t.Errorf("verdict = %+v, want revert %v and no wait", v, tc.wantRevert)
			}
			if !strings.Contains(v.Why, "first-round rmse") || !strings.Contains(v.Why, "12 beads") {
				t.Errorf("why = %q, want both rmses and the count named", v.Why)
			}
		})
	}
}

// TestStoryPointsAutoRevertJudgesEdges: under ten first-round samples the rank rule decides, a
// sample that landed at or before the adoption is not counted, no samples since the adoption
// wait, and a blend that fails is returned (gt-q2rrvb).
func TestStoryPointsAutoRevertJudgesEdges(t *testing.T) {
	t.Parallel()
	good := adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 3.5})
	inverted := adoptWeights(t, adoptTestWeights{boxes: -0.25, intercept: 1})
	rising := func(i int, _, _ float64) float64 { return float64(i) * 5 }

	t.Run("an inverted adoption on nine first-round samples reverts by rank", func(t *testing.T) {
		t.Parallel()
		v, err := judgeStoryPointsAdoption(autoRevertSamples(t, 9, true, inverted, good, rising), inverted, good, autoRevertAdoptedAt)
		if err != nil || !v.Revert || !strings.Contains(v.Why, "rank correlation") {
			t.Errorf("verdict = %+v, %v; want a revert by rank correlation", v, err)
		}
	})
	t.Run("a well ranked adoption on nine first-round samples is kept", func(t *testing.T) {
		t.Parallel()
		v, err := judgeStoryPointsAdoption(autoRevertSamples(t, 9, true, good, inverted, rising), good, inverted, autoRevertAdoptedAt)
		if err != nil || v.Revert || v.Waits {
			t.Errorf("verdict = %+v, %v; want it kept", v, err)
		}
	})
	t.Run("samples at or before the adoption are not counted", func(t *testing.T) {
		t.Parallel()
		old := autoRevertSamples(t, 12, true, inverted, good, rising)
		v, err := judgeStoryPointsAdoption(old, inverted, good, autoRevertAdoptedAt.Add(24*time.Hour))
		if err != nil || !v.Waits || v.Revert {
			t.Errorf("verdict = %+v, %v; want it to wait: no sample since", v, err)
		}
	})
	t.Run("no samples wait and name why", func(t *testing.T) {
		t.Parallel()
		v, err := judgeStoryPointsAdoption(nil, good, good, autoRevertAdoptedAt)
		if err != nil || !v.Waits || v.Revert || !strings.Contains(v.Why, "0") {
			t.Errorf("verdict = %+v, %v; want a wait naming the count", v, err)
		}
	})
	// A weights file whose embedding has another dimension fails the blend of a three-wide vector.
	wide := adoptWeights(t, adoptTestWeights{boxes: 0.25, intercept: 3.5, dim: trainTestDim + 2})
	t.Run("a failing blend of the adopted weights is returned", func(t *testing.T) {
		t.Parallel()
		if _, err := judgeStoryPointsAdoption(autoRevertSamples(t, 3, true, good, good, rising), wide, good, autoRevertAdoptedAt); err == nil {
			t.Error("err = nil, want the blend's error")
		}
	})
	t.Run("a failing blend of the previous weights is returned", func(t *testing.T) {
		t.Parallel()
		if _, err := judgeStoryPointsAdoption(autoRevertSamples(t, 3, true, good, good, rising), good, wide, autoRevertAdoptedAt); err == nil {
			t.Error("err = nil, want the blend's error")
		}
	})
}
