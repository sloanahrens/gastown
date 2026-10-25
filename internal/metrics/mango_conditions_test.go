package metrics

import (
	"fmt"
	"math"
	"testing"
)

// conditionRows builds the ten rows the acceptance fixture reads: every row is
// in rig gastown, the first six have lines in 10..60, and the first three use
// model a.
func conditionRows() []Row {
	rows := make([]Row, 10)
	for i := range rows {
		rows[i] = Row{
			"rig":   "gastown",
			"lines": "80",
			"model": "b",
		}
		if i < 6 {
			rows[i]["lines"] = "20"
		}
		if i < 3 {
			rows[i]["model"] = "a"
		}
	}
	return rows
}

// conditionCube is the three-condition box the acceptance fixture splits: a
// rig every row matches, a lines span six rows match and a model three match.
// The ranges are given out of column order, so a function that sorted them
// rather than following the box would fail the order check.
func conditionCube() Cube {
	return Cube{Ranges: []Range{
		{Column: "model", Categories: []string{"a"}},
		{Column: "lines", Min: 10, Max: 60},
		{Column: "rig", Categories: []string{"gastown"}},
	}}
}

// TestMangoConditionsChecksEveryShare splits a ten-row fixture three ways and
// reads each condition's share and narrows. It guards the loop's Matches call,
// the Describe of the one-range cube, and the share < MangoNarrowBelow test:
// change any of the three and one of these rows fails.
func TestMangoConditionsChecksEveryShare(t *testing.T) {
	t.Parallel()
	got := MangoConditions(conditionCube(), conditionRows())
	want := []MangoCondition{
		{Text: "model in {a}", Share: 0.3, Narrows: true},
		{Text: "lines in [10, 60]", Share: 0.6, Narrows: true},
		{Text: "rig in {gastown}", Share: 1.0, Narrows: false},
	}
	if len(got) != len(want) {
		t.Fatalf("conditions = %d, want one per box range (%d)", len(got), len(want))
	}
	for i := range want {
		if got[i].Text != want[i].Text {
			t.Errorf("condition %d text = %q, want the box's own order %q", i, got[i].Text, want[i].Text)
		}
		if got[i].Share != want[i].Share {
			t.Errorf("condition %d %q share = %v, want %v", i, got[i].Text, got[i].Share, want[i].Share)
		}
		if got[i].Narrows != want[i].Narrows {
			t.Errorf("condition %d %q narrows = %v, want %v", i, got[i].Text, got[i].Narrows, want[i].Narrows)
		}
	}
}

// TestMangoConditionsNarrowsBelowTheThreshold pins the boundary the panel
// draws on: a condition true of at least MangoNarrowBelow of the rows does not
// narrow, one row less does. The boundary is counted off the constant, so
// moving the threshold moves the table with it; what stays pinned is the strict
// < in narrows.
func TestMangoConditionsNarrowsBelowTheThreshold(t *testing.T) {
	t.Parallel()
	const total = 10
	atThreshold := int(math.Ceil(MangoNarrowBelow * total))
	tests := []struct {
		name    string
		matches int // rows of the ten out of "a", so share is matches/10
		narrows bool
	}{
		{"every row", total, false},
		{"at or above the threshold", atThreshold, false},
		{"one row under", atThreshold - 1, true},
		{"no matching rows", 0, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := make([]Row, total)
			for i := range rows {
				model := "b"
				if i < tc.matches {
					model = "a"
				}
				rows[i] = Row{"model": model}
			}
			got := MangoConditions(Cube{Ranges: []Range{{Column: "model", Categories: []string{"a"}}}}, rows)
			if len(got) != 1 {
				t.Fatalf("conditions = %d, want 1", len(got))
			}
			if want := float64(tc.matches) / total; got[0].Share != want {
				t.Errorf("share = %v, want %v", got[0].Share, want)
			}
			if got[0].Narrows != tc.narrows {
				t.Errorf("narrows = %v at share %v, want %v", got[0].Narrows, got[0].Share, tc.narrows)
			}
		})
	}
}

// TestMangoConditionsCapsTheConditions runs a box of MangoMaxConditions+2
// ranges: the function must stop at the cap and keep the box's order for the
// ones it keeps. It guards the break at i == MangoMaxConditions.
func TestMangoConditionsCapsTheConditions(t *testing.T) {
	t.Parallel()
	ranges := make([]Range, MangoMaxConditions+2)
	for i := range ranges {
		ranges[i] = Range{Column: fmt.Sprintf("c%02d", i), Min: 1, Max: 2}
	}
	got := MangoConditions(Cube{Ranges: ranges}, []Row{{"c00": "1"}})
	if len(got) != MangoMaxConditions {
		t.Fatalf("conditions = %d, want the cap %d", len(got), MangoMaxConditions)
	}
	if got[0].Text != "c00 in [1, 2]" {
		t.Errorf("first condition = %q, want the box's first range c00 in [1, 2]", got[0].Text)
	}
	if last := got[MangoMaxConditions-1].Text; last != "c15 in [1, 2]" {
		t.Errorf("last condition = %q, want the box's range %d, c15 in [1, 2]", last, MangoMaxConditions-1)
	}
}

// TestMangoConditionsHandlesTheEmptyBox checks the two degenerate inputs: a box
// with no ranges yields no conditions, and rows that are empty leave every
// share at 0 rather than dividing by zero, and narrow nothing.
func TestMangoConditionsHandlesTheEmptyBox(t *testing.T) {
	t.Parallel()
	if got := MangoConditions(Cube{}, conditionRows()); len(got) != 0 {
		t.Errorf("conditions of an empty box = %v, want none", got)
	}
	got := MangoConditions(conditionCube(), nil)
	if len(got) != len(conditionCube().Ranges) {
		t.Fatalf("conditions = %d, want one per box range", len(got))
	}
	for _, c := range got {
		if c.Share != 0 || c.Narrows {
			t.Errorf("%q with no rows = share %v narrows %v, want 0 and false", c.Text, c.Share, c.Narrows)
		}
	}
}
