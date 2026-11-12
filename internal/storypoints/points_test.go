package storypoints

import (
	"errors"
	"math"
	"testing"
)

// TestFromMinutesEdges pins the value on each edge, just below it and just
// above it. An edge belongs to the lower point; the value above it takes the
// next point up.
func TestFromMinutesEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		minutes float64
		want    int
	}{
		{"tiny positive is one", 0.01, 1},
		{"half a minute is one", 0.5, 1},
		{"just below edge 1.5", 1.49, 1},
		{"edge 1.5 is one", 1.5, 1},
		{"just above edge 1.5", 1.51, 2},
		{"just below edge 2.5", 2.49, 2},
		{"edge 2.5 is two", 2.5, 2},
		{"just above edge 2.5", 2.51, 3},
		{"just below edge 4", 3.99, 3},
		{"edge 4 is three", 4, 3},
		{"just above edge 4", 4.01, 5},
		{"just below edge 6.5", 6.49, 5},
		{"edge 6.5 is five", 6.5, 5},
		{"just above edge 6.5", 6.51, 8},
		{"just below edge 10.5", 10.49, 8},
		{"edge 10.5 is eight", 10.5, 8},
		{"just above edge 10.5", 10.51, 13},
		{"just below edge 17", 16.99, 13},
		{"edge 17 is thirteen", 17, 13},
		{"just above edge 17", 17.01, 21},
		{"just below edge 27.5", 27.49, 21},
		{"edge 27.5 is twenty-one", 27.5, 21},
		{"just above edge 27.5", 27.51, 34},
		{"just below edge 44.5", 44.49, 34},
		{"edge 44.5 is thirty-four", 44.5, 34},
		{"just above edge 44.5", 44.51, 55},
		{"just below edge 72", 71.99, 55},
		{"edge 72 is fifty-five", 72, 55},
		{"just above edge 72", 72.01, 89},
		{"a long task is eighty-nine", 1000, 89},
		{"infinity is eighty-nine", math.Inf(1), 89},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromMinutes(tt.minutes)
			if err != nil {
				t.Fatalf("FromMinutes(%v) error = %v, want nil", tt.minutes, err)
			}
			if got != tt.want {
				t.Fatalf("FromMinutes(%v) = %d, want %d", tt.minutes, got, tt.want)
			}
		})
	}
}

// TestFromMinutesRefusesNoMinutes: a zero, negative or NaN time is an absent
// measurement. It must return ErrNoMinutes and no point, never point 1.
func TestFromMinutesRefusesNoMinutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		minutes float64
	}{
		{"zero", 0},
		{"negative zero", math.Copysign(0, -1)},
		{"negative", -1},
		{"just below zero", -0.0001},
		{"negative infinity", math.Inf(-1)},
		{"NaN", math.NaN()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromMinutes(tt.minutes)
			if !errors.Is(err, ErrNoMinutes) {
				t.Fatalf("FromMinutes(%v) error = %v, want ErrNoMinutes", tt.minutes, err)
			}
			if got != 0 {
				t.Fatalf("FromMinutes(%v) = %d beside the error, want 0", tt.minutes, got)
			}
		})
	}
}

// TestEdgesMatchScale: every edge's point is on the scale, and the edges rise
// in both point and bound, so FromMinutes cannot skip a point.
func TestEdgesMatchScale(t *testing.T) {
	t.Parallel()
	wantPoints := []int{1, 2, 3, 5, 8, 13, 21, 34, 55, 89}
	edges := Edges()
	if len(edges) != len(wantPoints) {
		t.Fatalf("Edges() has %d edges, want %d", len(edges), len(wantPoints))
	}
	for i, e := range edges {
		if e.Points != wantPoints[i] {
			t.Fatalf("Edges()[%d].Points = %d, want %d", i, e.Points, wantPoints[i])
		}
		if i > 0 && e.UpTo <= edges[i-1].UpTo {
			t.Fatalf("Edges()[%d].UpTo = %v does not rise past %v", i, e.UpTo, edges[i-1].UpTo)
		}
	}
}

// TestEdgesReturnsACopy: a caller that edits the slice it got must not move
// the scale that FromMinutes uses.
func TestEdgesReturnsACopy(t *testing.T) {
	t.Parallel()
	edges := Edges()
	edges[0].Points = 99
	got, err := FromMinutes(1)
	if err != nil {
		t.Fatalf("FromMinutes(1) error = %v, want nil", err)
	}
	if got != 1 {
		t.Fatalf("FromMinutes(1) = %d after editing Edges(), want 1", got)
	}
}

func TestValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int
		want bool
	}{
		{0, false},
		{1, true},
		{2, true},
		{3, true},
		{4, false},
		{5, true},
		{6, false},
		{8, true},
		{13, true},
		{21, true},
		{34, true},
		{55, true},
		{89, true},
		{90, false},
		{-1, false},
	}
	for _, tt := range tests {
		if got := Valid(tt.n); got != tt.want {
			t.Errorf("Valid(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

func TestLabelBuilders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"estimate", SPLabel(3), "sp:3"},
		{"estimate of eighty-nine", SPLabel(89), "sp:89"},
		{"actual", ActualLabel(8), "sp-actual:8"},
		{"rung all", RungLabel("all"), "sp-rung:all"},
		{"rung shape-jev", RungLabel("shape-jev"), "sp-rung:shape-jev"},
		{"rung shape-embed", RungLabel("shape-embed"), "sp-rung:shape-embed"},
		{"rung shape", RungLabel("shape"), "sp-rung:shape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Fatalf("label = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// TestParseLabelRoundTrips: every label a builder writes parses back to the
// same kind and value.
func TestParseLabelRoundTrips(t *testing.T) {
	t.Parallel()
	var labels []string
	var wants []Label
	for _, e := range Edges() {
		labels = append(labels, SPLabel(e.Points), ActualLabel(e.Points))
		wants = append(wants,
			Label{Kind: KindEstimate, Points: e.Points},
			Label{Kind: KindActual, Points: e.Points})
	}
	for _, r := range rungs {
		labels = append(labels, RungLabel(r))
		wants = append(wants, Label{Kind: KindRung, Rung: r})
	}
	for _, m := range []float64{3, 12.5, 0.75} {
		labels = append(labels, MinutesLabel(m))
		wants = append(wants, Label{Kind: KindMinutes, Minutes: m})
	}
	for i, s := range labels {
		got, err := ParseLabel(s)
		if err != nil {
			t.Fatalf("ParseLabel(%q) error = %v, want nil", s, err)
		}
		if got != wants[i] {
			t.Fatalf("ParseLabel(%q) = %+v, want %+v", s, got, wants[i])
		}
	}
}

// TestParseLabelRejects: each spelling a builder would not write, or a value
// off the scale, is refused with ErrBadLabel.
func TestParseLabelRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		label string
	}{
		{"point not on the scale", "sp:4"},
		{"point zero", "sp:0"},
		{"point above the scale", "sp:90"},
		{"empty points", "sp:"},
		{"leading zero", "sp:05"},
		{"plus sign", "sp:+5"},
		{"negative points", "sp:-1"},
		{"non-digit points", "sp:x"},
		{"actual of zero", "sp-actual:0"},
		{"actual off the scale", "sp-actual:4"},
		{"actual with empty points", "sp-actual:"},
		{"actual with leading zero", "sp-actual:08"},
		{"rung with empty name", "sp-rung:"},
		{"rung not in the set", "sp-rung:bogus"},
		{"rung with wrong case", "sp-rung:ALL"},
		{"minutes of zero", "sp-minutes:0"},
		{"minutes negative", "sp-minutes:-2"},
		{"minutes with empty value", "sp-minutes:"},
		{"minutes not a number", "sp-minutes:soon"},
		{"minutes NaN", "sp-minutes:NaN"},
		{"minutes infinite", "sp-minutes:+Inf"},
		{"minutes with leading zero", "sp-minutes:012.5"},
		{"minutes in exponent form", "sp-minutes:1e3"},
		{"empty string", ""},
		{"unknown prefix", "sz:3"},
		{"bare number", "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLabel(tt.label)
			if !errors.Is(err, ErrBadLabel) {
				t.Fatalf("ParseLabel(%q) error = %v, want ErrBadLabel", tt.label, err)
			}
			if got != (Label{}) {
				t.Fatalf("ParseLabel(%q) = %+v beside the error, want zero Label", tt.label, got)
			}
		})
	}
}
