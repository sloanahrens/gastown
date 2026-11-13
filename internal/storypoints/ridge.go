package storypoints

import (
	"errors"
	"fmt"
	"math"
)

// ErrNotPositive reports a ridge system whose factorization breaks down: Cholesky
// meets a pivot that is not positive or not finite.
var ErrNotPositive = errors.New("storypoints: ridge system is not positive definite")

// ErrNonFinite reports a ridge value that is not a finite number: a NaN or Inf
// sample, target or coefficient.
var ErrNonFinite = errors.New("storypoints: ridge value is not a finite number")

// ridgeFit is one design, centered and ready to solve at any penalty. The Gram
// matrix is built once, so a search over penalties costs one factorization each.
// Primal form (XᵀX, d×d) is used when there are at least as many samples as
// features; dual form (XXᵀ, n×n) otherwise.
type ridgeFit struct {
	xc     [][]float64 // centered samples, n rows of d
	yc     []float64   // centered targets
	xm     []float64   // column means of the samples
	ym     float64     // mean of the targets
	primal bool
	gram   [][]float64 // lower triangle only
	xty    []float64   // primal only: Xᵀy
}

// newRidge centers x and y and builds the Gram matrix of the form ridge solves
// in. x has one row per sample; every row has the same length.
func newRidge(x [][]float64, y []float64) (*ridgeFit, error) {
	n := len(x)
	if n == 0 {
		return nil, errors.New("storypoints: ridge needs at least one sample")
	}
	if len(y) != n {
		return nil, fmt.Errorf("storypoints: ridge has %d samples and %d targets", n, len(y))
	}
	d := len(x[0])
	if d == 0 {
		return nil, errors.New("storypoints: ridge needs at least one feature")
	}
	r := &ridgeFit{
		xc:     make([][]float64, n),
		yc:     make([]float64, n),
		xm:     make([]float64, d),
		primal: d <= n,
	}
	for i, row := range x {
		if len(row) != d {
			return nil, fmt.Errorf("storypoints: ridge sample %d has %d features, want %d", i, len(row), d)
		}
		for j, v := range row {
			if !isFinite(v) {
				return nil, fmt.Errorf("storypoints: ridge sample %d feature %d: %w", i, j, ErrNonFinite)
			}
			r.xm[j] += v
		}
		if !isFinite(y[i]) {
			return nil, fmt.Errorf("storypoints: ridge target %d: %w", i, ErrNonFinite)
		}
		r.ym += y[i]
	}
	for j := range r.xm {
		r.xm[j] /= float64(n)
	}
	r.ym /= float64(n)
	for i, row := range x {
		c := make([]float64, d)
		for j, v := range row {
			c[j] = v - r.xm[j]
		}
		r.xc[i] = c
		r.yc[i] = y[i] - r.ym
	}
	if r.primal {
		r.gram = make([][]float64, d)
		for p := range r.gram {
			r.gram[p] = make([]float64, p+1)
		}
		r.xty = make([]float64, d)
		for i, row := range r.xc {
			for p, vp := range row {
				r.xty[p] += vp * r.yc[i]
				for q := 0; q <= p; q++ {
					r.gram[p][q] += vp * row[q]
				}
			}
		}
	} else {
		r.gram = make([][]float64, n)
		for i := range r.gram {
			r.gram[i] = make([]float64, i+1)
			for j := 0; j <= i; j++ {
				r.gram[i][j] = dot(r.xc[i], r.xc[j])
			}
		}
	}
	return r, nil
}

// solve returns the coefficients and intercept at penalty alpha: it minimizes
// sum (y_i - intercept - x_i·coef)² + alpha·|coef|². The solve is closed form, by
// Cholesky factorization of the normal equations (or of the kernel matrix when
// samples are fewer than features). The intercept is not penalized: it is the
// target mean less the coefficients on the feature means.
func (r *ridgeFit) solve(alpha float64) (coef []float64, intercept float64, err error) {
	if !(alpha > 0) || math.IsInf(alpha, 0) {
		return nil, 0, fmt.Errorf("storypoints: ridge penalty %v, want a positive number", alpha)
	}
	m := make([][]float64, len(r.gram))
	for i := range r.gram {
		m[i] = make([]float64, i+1)
		copy(m[i], r.gram[i])
		m[i][i] += alpha
	}
	if r.primal {
		coef, err = choleskySolve(m, r.xty)
		if err != nil {
			return nil, 0, err
		}
	} else {
		a, err := choleskySolve(m, r.yc)
		if err != nil {
			return nil, 0, err
		}
		coef = make([]float64, len(r.xm))
		for i, row := range r.xc {
			for p, v := range row {
				coef[p] += v * a[i]
			}
		}
	}
	// The coefficients are finite (choleskySolve checks them), and so are the
	// means of finite samples, so the intercept is finite too.
	intercept = r.ym - dot(r.xm, coef)
	return coef, intercept, nil
}

// choleskySolve solves m·x = b for the symmetric positive-definite m, given by
// its lower triangle (row i holds columns 0..i). m is overwritten by its
// factor, so the caller passes a copy.
func choleskySolve(m [][]float64, b []float64) ([]float64, error) {
	n := len(m)
	for j := 0; j < n; j++ {
		s := m[j][j]
		for k := 0; k < j; k++ {
			s -= m[j][k] * m[j][k]
		}
		if !(s > 0) || math.IsInf(s, 0) {
			return nil, ErrNotPositive
		}
		m[j][j] = math.Sqrt(s)
		for i := j + 1; i < n; i++ {
			t := m[i][j]
			for k := 0; k < j; k++ {
				t -= m[i][k] * m[j][k]
			}
			m[i][j] = t / m[j][j]
		}
	}
	z := make([]float64, n)
	for i := 0; i < n; i++ {
		t := b[i]
		for k := 0; k < i; k++ {
			t -= m[i][k] * z[k]
		}
		z[i] = t / m[i][i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		t := z[i]
		for k := i + 1; k < n; k++ {
			t -= m[k][i] * x[k]
		}
		x[i] = t / m[i][i]
	}
	for _, v := range x {
		if !isFinite(v) {
			return nil, ErrNonFinite
		}
	}
	return x, nil
}

// isFinite reports whether v is neither NaN nor an infinity.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// dot is the inner product of two equal-length vectors.
func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
