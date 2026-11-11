// Package storypoints maps a polecat's measured or predicted minutes onto a
// Fibonacci point, and reads and writes the labels that carry the point on a
// bead (gt-5d9k2f).
package storypoints

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ErrNoMinutes reports a time that is zero, negative or NaN. Such a time is an
// absent measurement, so it has no point; it is not the smallest point.
var ErrNoMinutes = errors.New("storypoints: minutes must be a positive number")

// ErrBadLabel reports a string that ParseLabel does not accept as a
// story-point label.
var ErrBadLabel = errors.New("storypoints: not a story-point label")

// Edge is one point on the scale. A time up to and including UpTo belongs to
// Points, unless the edge below already took it.
type Edge struct {
	Points int
	UpTo   float64
}

// edges runs from the smallest point up. The last edge has no upper bound.
var edges = []Edge{
	{Points: 1, UpTo: 1.5},
	{Points: 2, UpTo: 2.5},
	{Points: 3, UpTo: 4},
	{Points: 5, UpTo: 6.5},
	{Points: 8, UpTo: 10.5},
	{Points: 13, UpTo: 17},
	{Points: 21, UpTo: 27.5},
	{Points: 34, UpTo: 44.5},
	{Points: 55, UpTo: 72},
	{Points: 89, UpTo: math.Inf(1)},
}

// rungs names the inputs an estimate was made from, most inputs first.
var rungs = []string{"all", "shape-jev", "shape-embed", "shape"}

const (
	spPrefix     = "sp:"
	actualPrefix = "sp-actual:"
	rungPrefix   = "sp-rung:"
)

// Edges returns a copy of the scale, from the smallest point up.
func Edges() []Edge {
	return slices.Clone(edges)
}

// Valid reports whether n is a point on the scale.
func Valid(n int) bool {
	return slices.ContainsFunc(edges, func(e Edge) bool { return e.Points == n })
}

// FromMinutes returns the point for a time in minutes. A time on an edge takes
// the lower point. A time that is zero, negative or NaN returns ErrNoMinutes
// and no point.
func FromMinutes(m float64) (int, error) {
	if math.IsNaN(m) || m <= 0 {
		return 0, ErrNoMinutes
	}
	for _, e := range edges {
		if m <= e.UpTo {
			return e.Points, nil
		}
	}
	return edges[len(edges)-1].Points, nil
}

// SPLabel returns the label that carries an estimate, sp:N.
func SPLabel(n int) string {
	return spPrefix + strconv.Itoa(n)
}

// ActualLabel returns the label that carries the measured points, sp-actual:N.
func ActualLabel(n int) string {
	return actualPrefix + strconv.Itoa(n)
}

// RungLabel returns the label that names the inputs of an estimate,
// sp-rung:<rung>.
func RungLabel(rung string) string {
	return rungPrefix + rung
}

// LabelKind says which of the three label forms a Label is.
type LabelKind int

const (
	// KindEstimate is an sp:N label.
	KindEstimate LabelKind = iota + 1
	// KindActual is an sp-actual:N label.
	KindActual
	// KindRung is an sp-rung:<rung> label.
	KindRung
)

// Label is a parsed story-point label. Points is set for KindEstimate and
// KindActual; Rung is set for KindRung.
type Label struct {
	Kind   LabelKind
	Points int
	Rung   string
}

// ParseLabel parses a label that SPLabel, ActualLabel or RungLabel would write.
// Any other spelling is refused with ErrBadLabel, so sp:05 and sp:+5 are not
// read as sp:5.
func ParseLabel(s string) (Label, error) {
	if rest, ok := strings.CutPrefix(s, spPrefix); ok {
		n, err := parsePoints(rest)
		if err != nil || SPLabel(n) != s {
			return Label{}, badLabel(s)
		}
		return Label{Kind: KindEstimate, Points: n}, nil
	}
	if rest, ok := strings.CutPrefix(s, actualPrefix); ok {
		n, err := parsePoints(rest)
		if err != nil || ActualLabel(n) != s {
			return Label{}, badLabel(s)
		}
		return Label{Kind: KindActual, Points: n}, nil
	}
	if rest, ok := strings.CutPrefix(s, rungPrefix); ok {
		if !slices.Contains(rungs, rest) {
			return Label{}, badLabel(s)
		}
		return Label{Kind: KindRung, Rung: rest}, nil
	}
	return Label{}, badLabel(s)
}

// parsePoints reads the digits of a points label and accepts only a point on
// the scale.
func parsePoints(digits string) (int, error) {
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, ErrBadLabel
	}
	n, err := strconv.Atoi(digits)
	if err != nil || !Valid(n) {
		return 0, ErrBadLabel
	}
	return n, nil
}

func badLabel(s string) error {
	return fmt.Errorf("%w: %q", ErrBadLabel, s)
}
