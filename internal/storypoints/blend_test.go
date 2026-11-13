package storypoints

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

// flatJev is a knot table that maps every probability to 4 minutes, so a rung
// test does not depend on the interpolation.
var flatJev = Weights{knots: []knot{{0, 4}, {1, 4}}}

// sloped is a knot table with two interior knots, for the interpolation tests.
var sloped = Weights{knots: []knot{{0.2, 40}, {0.6, 10}}}

// ptr returns a pointer to v, for an optional input.
func ptr(v float64) *float64 {
	return &v
}

// geoMean is the geometric mean of xs, computed as a product root so the
// expected values do not repeat the implementation's log-space sum.
func geoMean(xs ...float64) float64 {
	prod := 1.0
	for _, x := range xs {
		prod *= x
	}
	return math.Pow(prod, 1/float64(len(xs)))
}

func TestBlendRungs(t *testing.T) {
	t.Parallel()
	// Shape 2, embedding 18, and Jev at 4 minutes give four distinct
	// averages, so a row that picks the wrong inputs fails on its value.
	tests := []struct {
		name       string
		in         Inputs
		wantMin    float64
		wantRung   string
		wantInputs []string
	}{
		{
			name:       "all three inputs",
			in:         Inputs{ShapeMinutes: ptr(2), EmbedMinutes: ptr(18), JevFits: ptr(0.3), Weights: flatJev},
			wantMin:    geoMean(2, 18, 4),
			wantRung:   "all",
			wantInputs: []string{"shape", "embed", "jev"},
		},
		{
			name:       "shape and jev",
			in:         Inputs{ShapeMinutes: ptr(2), JevFits: ptr(0.3), Weights: flatJev},
			wantMin:    geoMean(2, 4),
			wantRung:   "shape-jev",
			wantInputs: []string{"shape", "jev"},
		},
		{
			name:       "shape and embedding",
			in:         Inputs{ShapeMinutes: ptr(2), EmbedMinutes: ptr(18)},
			wantMin:    geoMean(2, 18),
			wantRung:   "shape-embed",
			wantInputs: []string{"shape", "embed"},
		},
		{
			name:       "shape only",
			in:         Inputs{ShapeMinutes: ptr(2)},
			wantMin:    2,
			wantRung:   "shape",
			wantInputs: []string{"shape"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Blend(tt.in)
			if err != nil {
				t.Fatalf("Blend error = %v, want nil", err)
			}
			if math.Abs(got.Minutes-tt.wantMin) > 1e-9 {
				t.Errorf("Minutes = %v, want %v", got.Minutes, tt.wantMin)
			}
			if got.Rung != tt.wantRung {
				t.Errorf("Rung = %q, want %q", got.Rung, tt.wantRung)
			}
			if !slices.Equal(got.Inputs, tt.wantInputs) {
				t.Errorf("Inputs = %q, want %q", got.Inputs, tt.wantInputs)
			}
		})
	}
}

func TestBlendPointsFollowFromMinutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		shape   float64
		wantPts int
	}{
		// An average on an edge takes the lower point, as FromMinutes does.
		{"average on the four-minute edge is three", 4, 3},
		{"average above the four-minute edge is five", 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Blend(Inputs{ShapeMinutes: ptr(tt.shape)})
			if err != nil {
				t.Fatalf("Blend error = %v, want nil", err)
			}
			if got.Points != tt.wantPts {
				t.Errorf("Points = %d, want %d", got.Points, tt.wantPts)
			}
		})
	}
}

func TestJevMinutesInterpolatesAndClips(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		p    float64
		want float64
	}{
		{"zero clips to the first knot", 0, 40},
		{"the first knot is its own minutes", 0.2, 40},
		{"midpoint interpolates", 0.4, 25},
		{"three quarters of the way interpolates", 0.5, 17.5},
		{"the last knot is its own minutes", 0.6, 10},
		{"one clips to the last knot", 1, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sloped.JevMinutes(tt.p)
			if err != nil {
				t.Fatalf("JevMinutes(%v) error = %v, want nil", tt.p, err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("JevMinutes(%v) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}

func TestBlendRefusals(t *testing.T) {
	t.Parallel()
	// wantNamed is the input the error must name. Naming it tells the caller
	// which estimate was refused, not only that some minutes were bad.
	tests := []struct {
		name      string
		in        Inputs
		wantErr   error
		wantNamed string
	}{
		{"no input at all", Inputs{Weights: flatJev}, ErrNoInputs, ""},
		{"jev without shape", Inputs{JevFits: ptr(0.5), Weights: flatJev}, ErrNoShape, ""},
		{"embedding without shape", Inputs{EmbedMinutes: ptr(8)}, ErrNoShape, ""},
		{"zero shape minutes", Inputs{ShapeMinutes: ptr(0)}, ErrNoMinutes, "shape input"},
		{"negative shape minutes", Inputs{ShapeMinutes: ptr(-3)}, ErrNoMinutes, "shape input"},
		{"NaN shape minutes", Inputs{ShapeMinutes: ptr(math.NaN())}, ErrNoMinutes, "shape input"},
		{"infinite shape minutes", Inputs{ShapeMinutes: ptr(math.Inf(1))}, ErrNoMinutes, "shape input"},
		{"zero embedding minutes", Inputs{ShapeMinutes: ptr(2), EmbedMinutes: ptr(0)}, ErrNoMinutes, "embed input"},
		{"negative embedding minutes", Inputs{ShapeMinutes: ptr(2), EmbedMinutes: ptr(-1)}, ErrNoMinutes, "embed input"},
		{"jev probability below zero", Inputs{ShapeMinutes: ptr(2), JevFits: ptr(-0.01), Weights: flatJev}, ErrBadProbability, ""},
		{"jev probability above one", Inputs{ShapeMinutes: ptr(2), JevFits: ptr(1.01), Weights: flatJev}, ErrBadProbability, ""},
		{"jev probability NaN", Inputs{ShapeMinutes: ptr(2), JevFits: ptr(math.NaN()), Weights: flatJev}, ErrBadProbability, ""},
		{"jev without a knot table", Inputs{ShapeMinutes: ptr(2), JevFits: ptr(0.5)}, ErrNoWeights, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Blend(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Blend error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantNamed != "" && !strings.Contains(err.Error(), tt.wantNamed) {
				t.Errorf("Blend error = %q, want it to name %q", err, tt.wantNamed)
			}
			if got.Rung != "" || got.Minutes != 0 {
				t.Errorf("Blend returned %+v with the error; want the zero value", got)
			}
		})
	}
}

func TestBlendRefusesAverageWithNoPoint(t *testing.T) {
	t.Parallel()
	noPoint := func(float64) (int, error) { return 0, ErrNoMinutes }
	got, err := blendWith(Inputs{ShapeMinutes: ptr(2)}, noPoint)
	if !errors.Is(err, ErrNoMinutes) {
		t.Fatalf("blendWith error = %v, want ErrNoMinutes", err)
	}
	if got.Points != 0 || got.Rung != "" {
		t.Errorf("blendWith returned %+v with the error; want the zero value", got)
	}
}

func TestJevMinutesRefusesBadProbability(t *testing.T) {
	t.Parallel()
	for _, p := range []float64{-0.5, 2, math.NaN()} {
		_, err := sloped.JevMinutes(p)
		if !errors.Is(err, ErrBadProbability) {
			t.Errorf("JevMinutes(%v) error = %v, want ErrBadProbability", p, err)
		}
	}
}
