package spendcap

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// noon is the fixed now every case evaluates against: the middle of a UTC day.
func noon() time.Time {
	return time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
}

// closeEnough compares money at the epsilon the package compares caps with, so
// a case can state 0.3 for a sum of floats.
func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < epsilon
}

func TestUnderAtOverTheCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		capUSD    float64
		costs     []float64
		wantLevel Level
		wantSpent float64
		wantLeft  float64
	}{
		{
			name: "just below the cap", capUSD: 10,
			costs:     []float64{4.75, 4.875},
			wantLevel: Under, wantSpent: 9.625, wantLeft: 0.375,
		},
		{
			name: "exactly at the cap", capUSD: 10,
			costs:     []float64{4.75, 5.25},
			wantLevel: At, wantSpent: 10, wantLeft: 0,
		},
		{
			name: "above the cap", capUSD: 10,
			costs:     []float64{4.75, 5.5},
			wantLevel: Over, wantSpent: 10.25, wantLeft: 0,
		},
		{
			name: "at the cap through a float sum", capUSD: 0.3,
			costs:     []float64{0.1, 0.1, 0.1},
			wantLevel: At, wantSpent: 0.3, wantLeft: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var spend []Record
			for _, cost := range c.costs {
				spend = append(spend, Record{
					Workspace: "acme",
					EndedAt:   noon().Add(-time.Hour),
					CostUSD:   cost,
				})
			}
			got := Evaluate(Cap{Workspace: "acme", DailyUSD: c.capUSD}, spend, noon())
			if got.Level != c.wantLevel {
				t.Errorf("Level = %q, want %q", got.Level, c.wantLevel)
			}
			if !closeEnough(got.Spent, c.wantSpent) {
				t.Errorf("Spent = %v, want %v", got.Spent, c.wantSpent)
			}
			if !closeEnough(got.Remaining, c.wantLeft) {
				t.Errorf("Remaining = %v, want %v", got.Remaining, c.wantLeft)
			}
		})
	}
}

func TestOtherWorkspacesAndOtherDaysIgnored(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	cases := []struct {
		name   string
		record Record
		want   float64
	}{
		{
			name:   "the same workspace in the day",
			record: Record{Workspace: "acme", EndedAt: start.Add(12 * time.Hour), CostUSD: 2},
			want:   2,
		},
		{
			name:   "another workspace in the day",
			record: Record{Workspace: "other", EndedAt: start.Add(12 * time.Hour), CostUSD: 2},
			want:   0,
		},
		{
			name:   "the day before",
			record: Record{Workspace: "acme", EndedAt: start.Add(-time.Nanosecond), CostUSD: 2},
			want:   0,
		},
		{
			name:   "the day after",
			record: Record{Workspace: "acme", EndedAt: end, CostUSD: 2},
			want:   0,
		},
		{
			name:   "the first instant of the day",
			record: Record{Workspace: "acme", EndedAt: start, CostUSD: 2},
			want:   2,
		},
		{
			name:   "the last instant of the day",
			record: Record{Workspace: "acme", EndedAt: end.Add(-time.Nanosecond), CostUSD: 2},
			want:   2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(Cap{Workspace: "acme", DailyUSD: 10}, []Record{c.record}, noon())
			if !closeEnough(got.Spent, c.want) {
				t.Errorf("Spent = %v, want %v", got.Spent, c.want)
			}
			if got.Level != Under {
				t.Errorf("Level = %q, want %q", got.Level, Under)
			}
		})
	}
}

// A cost or cap that is not a number cannot be judged against the other, so the
// day is Unknown rather than Under: a dispatch must not be allowed on it.
func TestNotANumberIsUnknown(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	cases := []struct {
		name   string
		capUSD float64
		costs  []float64
	}{
		{name: "a NaN cost over a cap", capUSD: 10, costs: []float64{4, nan}},
		{name: "a NaN cost with no cap", capUSD: 0, costs: []float64{nan}},
		{name: "a NaN cap", capUSD: nan, costs: []float64{4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var spend []Record
			for _, cost := range c.costs {
				spend = append(spend, Record{Workspace: "acme", EndedAt: noon().Add(-time.Hour), CostUSD: cost})
			}
			got := Evaluate(Cap{Workspace: "acme", DailyUSD: c.capUSD}, spend, noon())
			if got.Level != Unknown {
				t.Errorf("Level = %q, want %q", got.Level, Unknown)
			}
		})
	}
}

func TestZeroCapMeansNoCap(t *testing.T) {
	t.Parallel()
	spend := []Record{
		{Workspace: "acme", EndedAt: noon().Add(-time.Hour), CostUSD: 5},
		{Workspace: "other", EndedAt: noon().Add(-time.Hour), CostUSD: 99},
	}
	for _, daily := range []float64{0, -1} {
		t.Run(fmt.Sprintf("daily %g", daily), func(t *testing.T) {
			t.Parallel()
			got := Evaluate(Cap{Workspace: "acme", DailyUSD: daily}, spend, noon())
			if got.Level != Under {
				t.Errorf("Level = %q, want %q", got.Level, Under)
			}
			if got.Spent != 5 {
				t.Errorf("Spent = %v, want 5", got.Spent)
			}
			if got.Remaining != 0 {
				t.Errorf("Remaining = %v, want 0", got.Remaining)
			}
		})
	}
}
