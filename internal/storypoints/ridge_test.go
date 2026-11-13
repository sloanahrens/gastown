package storypoints

import (
	"errors"
	"math"
	"testing"
)

// fitRidge is newRidge and solve in one call: the fit the tests check.
func fitRidge(x [][]float64, y []float64, alpha float64) ([]float64, float64, error) {
	r, err := newRidge(x, y)
	if err != nil {
		return nil, 0, err
	}
	return r.solve(alpha)
}

// ridgeRows is n deterministic samples of d features, centered so the feature
// means are zero and the intercept of the target is its own mean.
func ridgeRows(n, d int) [][]float64 {
	x := make([][]float64, n)
	for i := range x {
		x[i] = make([]float64, d)
		for j := range x[i] {
			x[i][j] = math.Sin(float64((i+1)*(j+3))) + 0.1*float64(i%7)
		}
	}
	for j := 0; j < d; j++ {
		var mean float64
		for i := range x {
			mean += x[i][j]
		}
		mean /= float64(n)
		for i := range x {
			x[i][j] -= mean
		}
	}
	return x
}

// TestRidgeRecoversKnownCoefficients: a noiseless linear target with more samples
// than features is fitted at a tiny penalty to the coefficients that made it.
func TestRidgeRecoversKnownCoefficients(t *testing.T) {
	t.Parallel()
	beta := []float64{2, -1, 0.5}
	const intercept = 3.0
	x := ridgeRows(40, len(beta))
	// Shift every feature off zero, so the intercept must absorb the feature means.
	for i := range x {
		for j := range x[i] {
			x[i][j] += 5
		}
	}
	y := make([]float64, len(x))
	for i, row := range x {
		y[i] = intercept + dot(row, beta)
	}
	coef, got, err := fitRidge(x, y, 1e-9)
	if err != nil {
		t.Fatalf("fitRidge: %v", err)
	}
	for j := range beta {
		if math.Abs(coef[j]-beta[j]) > 1e-6 {
			t.Errorf("coef[%d] = %v, want %v", j, coef[j], beta[j])
		}
	}
	if math.Abs(got-intercept) > 1e-6 {
		t.Errorf("intercept = %v, want %v", got, intercept)
	}
}

// TestRidgeDualFormRecoversKnownCoefficients: with fewer samples than features the
// kernel form is used. The target's coefficients lie in the span of the samples,
// so the least-norm interpolating fit is exactly those coefficients.
func TestRidgeDualFormRecoversKnownCoefficients(t *testing.T) {
	t.Parallel()
	const n, d = 5, 9
	x := ridgeRows(n, d)
	weights := []float64{1, -2, 0.5, 3, -0.25}
	beta := make([]float64, d)
	for i, row := range x {
		for j, v := range row {
			beta[j] += weights[i] * v
		}
	}
	y := make([]float64, n)
	for i, row := range x {
		y[i] = 4 + dot(row, beta)
	}
	coef, got, err := fitRidge(x, y, 1e-10)
	if err != nil {
		t.Fatalf("fitRidge: %v", err)
	}
	for j := range beta {
		if math.Abs(coef[j]-beta[j]) > 1e-6 {
			t.Errorf("coef[%d] = %v, want %v", j, coef[j], beta[j])
		}
	}
	if math.Abs(got-4) > 1e-6 {
		t.Errorf("intercept = %v, want 4", got)
	}
}

// TestRidgeRefusesBadInput: each input the solve cannot use is an error, not a
// fitted answer.
func TestRidgeRefusesBadInput(t *testing.T) {
	t.Parallel()
	good := ridgeRows(6, 2)
	goodY := []float64{1, 2, 3, 4, 5, 6}
	nanRows := ridgeRows(6, 2)
	nanRows[3][1] = math.NaN()
	// Five samples of nine features take the dual form, so a NaN there goes through
	// the kernel factorization rather than the normal equations.
	wide := ridgeRows(5, 9)
	wide[2][3] = math.NaN()
	wideY := []float64{1, 2, 3, 4, 5}
	nanTarget := []float64{1, 2, math.NaN(), 4, 5, 6}
	infRows := ridgeRows(6, 2)
	infRows[0][0] = math.Inf(-1)
	// Two samples of 1e200 overflow the Gram matrix to +Inf while every input is a
	// finite number, so the factorization is what breaks down.
	overflow := [][]float64{{1e200}, {-1e200}}
	cases := []struct {
		name  string
		x     [][]float64
		y     []float64
		alpha float64
		want  error
	}{
		{"no samples", nil, nil, 1, nil},
		{"target count differs", good, goodY[:3], 1, nil},
		{"ragged row", [][]float64{{1, 2}, {3}}, []float64{1, 2}, 1, nil},
		{"no features", [][]float64{{}, {}}, []float64{1, 2}, 1, nil},
		{"zero penalty", good, goodY, 0, nil},
		{"negative penalty", good, goodY, -1, nil},
		{"infinite penalty", good, goodY, math.Inf(1), nil},
		{"NaN feature", nanRows, goodY, 1, ErrNonFinite},
		{"NaN feature, dual form", wide, wideY, 1, ErrNonFinite},
		{"NaN target", good, nanTarget, 1, ErrNonFinite},
		{"infinite feature", infRows, goodY, 1, ErrNonFinite},
		{"finite inputs whose Gram matrix overflows", overflow, []float64{1, 2}, 1, ErrNotPositive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := fitRidge(tc.x, tc.y, tc.alpha)
			if err == nil {
				t.Fatalf("fitRidge returned no error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
