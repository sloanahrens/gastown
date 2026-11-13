package storypoints

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrNoInputs reports a Blend call with no input at all.
var ErrNoInputs = errors.New("storypoints: blend has no inputs")

// ErrNoShape reports a Blend call whose shape minutes are absent. Shape is
// always available, so a blend without it is a caller bug, not a rung.
var ErrNoShape = errors.New("storypoints: blend needs the shape minutes")

// ErrBadProbability reports a fits_twenty_minutes probability that is NaN or
// outside 0 to 1.
var ErrBadProbability = errors.New("storypoints: probability must be between 0 and 1")

// Inputs are the estimates a blend may use. A nil pointer is an input that is
// not available. Shape is required; embedding and Jev are optional. Weights
// holds the knot table that maps the Jev probability to minutes.
type Inputs struct {
	ShapeMinutes *float64
	EmbedMinutes *float64
	JevFits      *float64
	Weights      Weights
}

// Blended is one estimate: the averaged minutes, the point they round to, the
// inputs that were averaged, and the rung they name.
type Blended struct {
	Minutes float64
	Points  int
	Inputs  []string
	Rung    string
}

// Blend averages the inputs that are present on the log scale with equal
// weight. Every minutes input must be finite and positive, and the Jev
// probability must lie in 0 to 1; any other input is refused before the
// average is taken.
func Blend(in Inputs) (Blended, error) {
	return blendWith(in, FromMinutes)
}

// blendWith is Blend with the step that turns the blended minutes into a point
// passed in. A blend of validated inputs reaches a zero average only at the
// bottom of the float range, so the refusal for a time with no point is
// exercised through this seam.
func blendWith(in Inputs, toPoints func(float64) (int, error)) (Blended, error) {
	if in.ShapeMinutes == nil && in.EmbedMinutes == nil && in.JevFits == nil {
		return Blended{}, ErrNoInputs
	}
	if in.ShapeMinutes == nil {
		return Blended{}, ErrNoShape
	}

	type term struct {
		name    string
		minutes float64
	}
	terms := []term{{"shape", *in.ShapeMinutes}}
	if in.EmbedMinutes != nil {
		terms = append(terms, term{"embed", *in.EmbedMinutes})
	}
	if in.JevFits != nil {
		m, err := in.Weights.JevMinutes(*in.JevFits)
		if err != nil {
			return Blended{}, fmt.Errorf("storypoints: jev input: %w", err)
		}
		terms = append(terms, term{"jev", m})
	}

	names := make([]string, 0, len(terms))
	var logSum float64
	for _, t := range terms {
		if !validMinutes(t.minutes) {
			return Blended{}, fmt.Errorf("%w: %s input is %v", ErrNoMinutes, t.name, t.minutes)
		}
		logSum += math.Log(t.minutes)
		names = append(names, t.name)
	}

	minutes := math.Exp(logSum / float64(len(terms)))
	points, err := toPoints(minutes)
	if err != nil {
		return Blended{}, err
	}
	return Blended{
		Minutes: minutes,
		Points:  points,
		Inputs:  names,
		Rung:    rungFor(in.EmbedMinutes != nil, in.JevFits != nil),
	}, nil
}

// rungFor names the rung for the optional inputs that are present.
func rungFor(embed, jev bool) string {
	switch {
	case embed && jev:
		return "all"
	case jev:
		return "shape-jev"
	case embed:
		return "shape-embed"
	}
	return "shape"
}

// validMinutes reports whether m is a finite, positive number of minutes.
func validMinutes(m float64) bool {
	return m > 0 && !math.IsInf(m, 1)
}

// JevMinutes maps a fits_twenty_minutes probability to minutes by linear
// interpolation on the knot table. A probability below the first knot or above
// the last takes that knot's minutes. A probability that is NaN or outside 0
// to 1 returns ErrBadProbability; a Weights with no knot table returns
// ErrNoWeights.
func (w Weights) JevMinutes(p float64) (float64, error) {
	if !(p >= 0 && p <= 1) {
		return 0, fmt.Errorf("%w: %v", ErrBadProbability, p)
	}
	if len(w.knots) == 0 {
		return 0, ErrNoWeights
	}
	// j is the first knot at or above p. Below the first knot and above the
	// last both clip to that end.
	j := sort.Search(len(w.knots), func(i int) bool { return w.knots[i].fits >= p })
	switch {
	case j == len(w.knots):
		return w.knots[j-1].minutes, nil
	case j == 0 || w.knots[j].fits == p:
		return w.knots[j].minutes, nil
	}
	lo, hi := w.knots[j-1], w.knots[j]
	t := (p - lo.fits) / (hi.fits - lo.fits)
	return lo.minutes + t*(hi.minutes-lo.minutes), nil
}
