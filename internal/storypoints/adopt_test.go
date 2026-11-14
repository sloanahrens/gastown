package storypoints

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// swapRanks is the ranking 1..n with each pair of 1-based positions swapped. The
// pairs a case passes are disjoint, so the ranking's sum of squared rank differences
// from the identity is the sum of each swap's 2*d*d, and the correlation the case
// expects follows from that sum alone.
func swapRanks(n int, swaps ...[2]int) []float64 {
	r := make([]float64, n)
	for i := range r {
		r[i] = float64(i + 1)
	}
	for _, s := range swaps {
		i, j := s[0]-1, s[1]-1
		r[i], r[j] = r[j], r[i]
	}
	return r
}

// noTieRho is the rank correlation of two rankings with no tied values, from its
// closed form: 1 - 6*sum(d^2)/(n*(n^2-1)). It is the test's ground truth and shares
// no code with the ranks and sort the rule uses.
func noTieRho(actual, predicted []float64) float64 {
	var sum float64
	for i := range actual {
		d := actual[i] - predicted[i]
		sum += d * d
	}
	n := float64(len(actual))
	return 1 - 6*sum/(n*(n*n-1))
}

// holdoutOf pairs measured minutes that rise with position, 1..len(proposed), with
// the rankings the two models predict for them.
func holdoutOf(proposed, checked []float64) []Holdout {
	h := make([]Holdout, len(proposed))
	for i := range h {
		h[i] = Holdout{Actual: float64(i + 1), Proposed: proposed[i], Checked: checked[i]}
	}
	return h
}

// column returns one value of each holdout bead, in bead order.
func column(holdout []Holdout, value func(Holdout) float64) []float64 {
	v := make([]float64, len(holdout))
	for i, h := range holdout {
		v[i] = value(h)
	}
	return v
}

// TestAdoptTable: the decision the rule makes on a measurable holdout, and the one
// line that names why. The rankings are 25 beads' worth, built as disjoint swaps so
// the closed form noTieRho derives each correlation from the beads themselves: a
// swap of distance d moves the sum of squared rank differences by 2*d*d.
func TestAdoptTable(t *testing.T) {
	t.Parallel()
	// sum of squared rank differences 988, correlation 0.620.
	ranks62 := swapRanks(25,
		[2]int{1, 8}, [2]int{2, 9}, [2]int{3, 10}, [2]int{4, 11}, [2]int{5, 12},
		[2]int{6, 13}, [2]int{7, 14}, // seven distance-7 swaps: 7 * 2*49
		[2]int{15, 25},                 // distance 10: 2*100
		[2]int{16, 23},                 // distance 7: 2*49
		[2]int{17, 18}, [2]int{19, 20}, // two adjacent swaps: 2 each
	)
	// sum 1040, correlation 0.600.
	ranks60 := swapRanks(25,
		[2]int{1, 17}, [2]int{2, 18}, // distance 16 each: 2*256 twice
		[2]int{3, 5}, [2]int{6, 8}, // distance 2 each: 2*4 twice
	)
	// sum 936, correlation 0.640.
	ranks64 := swapRanks(25,
		[2]int{1, 21}, // distance 20: 2*400
		[2]int{2, 10}, // distance 8: 2*64
		[2]int{3, 5},  // distance 2: 2*4
	)
	// sum 910, correlation 0.650.
	ranks65 := swapRanks(25,
		[2]int{1, 21},                  // distance 20: 2*400
		[2]int{2, 9},                   // distance 7: 2*49
		[2]int{3, 5},                   // distance 2: 2*4
		[2]int{11, 12}, [2]int{13, 14}, // two adjacent swaps: 2 each
	)
	// sum 1170, correlation 0.550.
	ranks55 := swapRanks(25,
		[2]int{1, 25}, // distance 24: 2*576
		[2]int{2, 5},  // distance 3: 2*9
	)
	// sum 1700, correlation 0.346: below the 0.4 bar.
	ranksBelowBar := swapRanks(25,
		[2]int{1, 25},              // distance 24: 2*576
		[2]int{2, 18},              // distance 16: 2*256
		[2]int{3, 6}, [2]int{4, 7}, // distance 3 each: 2*9 twice
	)
	// sum 520, correlation 0.800, and sum 598, correlation 0.770: the second is
	// exactly the margin below the first.
	ranks80 := swapRanks(25,
		[2]int{1, 17}, // distance 16: 2*256
		[2]int{2, 4},  // distance 2: 2*4
	)
	ranks77 := swapRanks(25,
		[2]int{1, 18}, // distance 17: 2*289
		[2]int{2, 5},  // distance 3: 2*9
		[2]int{3, 4},  // adjacent: 2
	)

	cases := []struct {
		name        string
		holdout     []Holdout
		wantAdopted bool
		// The correlations noTieRho must derive from the holdout before the rule is
		// called. A case with tied values has no closed form and leaves these empty.
		wantProposed, wantChecked string
		wantReason                []string
	}{
		{
			name:         "proposed 0.62 against the current 0.60",
			holdout:      holdoutOf(ranks62, ranks60),
			wantAdopted:  true,
			wantProposed: "0.620", wantChecked: "0.600",
			wantReason: []string{"adopted", "0.620", "0.600", "0.40"},
		},
		{
			name:         "below the bar",
			holdout:      holdoutOf(ranksBelowBar, ranks60),
			wantAdopted:  false,
			wantProposed: "0.346", wantChecked: "0.600",
			wantReason: []string{"rejected", "0.346", "0.40"},
		},
		{
			name:         "more than the margin worse than the current weights",
			holdout:      holdoutOf(ranks55, ranks65),
			wantAdopted:  false,
			wantProposed: "0.550", wantChecked: "0.650",
			wantReason: []string{"rejected", "0.550", "0.650", "0.03"},
		},
		{
			name:         "exactly the margin worse is adopted",
			holdout:      holdoutOf(ranks62, ranks65),
			wantAdopted:  true,
			wantProposed: "0.620", wantChecked: "0.650",
			wantReason: []string{"adopted", "0.620", "0.650"},
		},
		{
			name:         "worse but inside the margin",
			holdout:      holdoutOf(ranks62, ranks64),
			wantAdopted:  true,
			wantProposed: "0.620", wantChecked: "0.640",
			wantReason: []string{"adopted", "0.620", "0.640"},
		},
		{
			// The two correlations are exactly 0.03 apart in arithmetic and
			// 0.030000000000000027 apart in float64, so the margin's tolerance
			// decides this one: it is adopted.
			name:         "exactly the margin worse, at the float boundary",
			holdout:      holdoutOf(ranks77, ranks80),
			wantAdopted:  true,
			wantProposed: "0.770", wantChecked: "0.800",
			wantReason: []string{"adopted", "0.770", "0.800"},
		},
		{
			name:        "holdout under the minimum size",
			holdout:     holdoutOf(ranks62[:MinAdoptHoldout-1], ranks60[:MinAdoptHoldout-1]),
			wantAdopted: false,
			wantReason:  []string{"rejected", "9 beads", "10", "not measurable"},
		},
		{
			// Five of the proposed values are the same, so each takes the mean of the
			// ranks they span, 8, whatever order they arrive in. That ranks the
			// proposal at -0.937 against the measured minutes, below the bar; a tie
			// handling that gave them distinct ranks would score it differently.
			name: "tied proposed values share a mean rank",
			holdout: holdoutOf(
				[]float64{10, 10, 10, 10, 10, 5, 4, 3, 2, 1},
				[]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			),
			wantAdopted: false,
			wantReason:  []string{"rejected", "-0.937", "0.40"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.wantProposed != "" {
				actual := column(tc.holdout, func(h Holdout) float64 { return h.Actual })
				proposed := column(tc.holdout, func(h Holdout) float64 { return h.Proposed })
				checked := column(tc.holdout, func(h Holdout) float64 { return h.Checked })
				if got := fmt.Sprintf("%.3f", noTieRho(actual, proposed)); got != tc.wantProposed {
					t.Fatalf("the case's proposed ranking correlates %s, want %s", got, tc.wantProposed)
				}
				if got := fmt.Sprintf("%.3f", noTieRho(actual, checked)); got != tc.wantChecked {
					t.Fatalf("the case's current ranking correlates %s, want %s", got, tc.wantChecked)
				}
			}
			got := Adopt(tc.holdout)
			if got.Adopted != tc.wantAdopted {
				t.Fatalf("Adopt = %+v, want adopted %v", got, tc.wantAdopted)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(got.Reason, want) {
					t.Errorf("reason %q does not name %q", got.Reason, want)
				}
			}
			if strings.Contains(got.Reason, "\n") {
				t.Errorf("reason is not one line: %q", got.Reason)
			}
		})
	}
}

// TestAdoptRefusesAnUnmeasurableHoldout: every holdout no rank correlation can be
// read from is a rejection that names what is missing, never an adoption. A series
// with no spread is not a correlation of zero, and a value that is not a positive
// finite number stops the rule before any ranking.
func TestAdoptRefusesAnUnmeasurableHoldout(t *testing.T) {
	t.Parallel()
	good := func() []Holdout {
		h := make([]Holdout, 12)
		for i := range h {
			h[i] = Holdout{Actual: float64(i + 1), Proposed: float64(i + 1), Checked: float64(i + 1)}
		}
		return h
	}
	cases := []struct {
		name   string
		mutate func([]Holdout)
		want   []string
	}{
		{
			name: "proposed blend is one value on every bead",
			mutate: func(h []Holdout) {
				for i := range h {
					h[i].Proposed = 5
				}
			},
			want: []string{"rejected", "proposed blend is one value", "not measurable"},
		},
		{
			name: "current blend is one value on every bead",
			mutate: func(h []Holdout) {
				for i := range h {
					h[i].Checked = 5
				}
			},
			want: []string{"rejected", "current weights' blend is one value", "cannot be made"},
		},
		{
			name: "measured minutes are one value on every bead",
			mutate: func(h []Holdout) {
				for i := range h {
					h[i].Actual = 5
				}
			},
			want: []string{"rejected", "measured minutes are one value", "no rank correlation is measurable"},
		},
		{
			name:   "NaN proposed minutes",
			mutate: func(h []Holdout) { h[3].Proposed = math.NaN() },
			want:   []string{"rejected", "bead 3", "proposed", "not measurable"},
		},
		{
			name:   "zero measured minutes",
			mutate: func(h []Holdout) { h[0].Actual = 0 },
			want:   []string{"rejected", "bead 0", "measured", "positive finite"},
		},
		{
			name:   "infinite current minutes",
			mutate: func(h []Holdout) { h[7].Checked = math.Inf(1) },
			want:   []string{"rejected", "bead 7", "current", "positive finite"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := good()
			tc.mutate(h)
			got := Adopt(h)
			if got.Adopted {
				t.Fatalf("Adopt adopted a holdout whose %s", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Reason, want) {
					t.Errorf("reason %q does not name %q", got.Reason, want)
				}
			}
		})
	}
}
