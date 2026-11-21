package daemon

import (
	"math"
	"testing"
	"time"
)

// TestJSONLTableBudget: the budget follows rows, per-row time and slowdown,
// doubled by the margin, and is clamped to the floor and the ceiling.
func TestJSONLTableBudget(t *testing.T) {
	t.Parallel()
	const perRow = 150 * time.Microsecond
	const floor = time.Minute
	cases := []struct {
		name     string
		rows     int
		perRow   time.Duration
		slowdown float64
		floor    time.Duration
		ceiling  time.Duration
		want     time.Duration
	}{
		{"the issues table at normal speed is the floor", 10886, perRow, 1, floor, jsonlIssuesBudgetCeiling, 60 * time.Second},
		{"the issues table at 140 times slowdown", 10886, perRow, 140, floor, jsonlIssuesBudgetCeiling, 457212 * time.Millisecond},
		{"a slowdown of 1000 is capped at the ceiling", 10886, perRow, 1000, floor, jsonlIssuesBudgetCeiling, jsonlIssuesBudgetCeiling},
		{"zero rows give the floor", 0, perRow, 1, floor, jsonlIssuesBudgetCeiling, floor},
		{"a negative per-row time gives the floor", 10886, -perRow, 1, floor, jsonlIssuesBudgetCeiling, floor},
		{"a slowdown of 0.2 counts as 1", 10886, perRow, 0.2, floor, jsonlIssuesBudgetCeiling, floor},
		{"a NaN slowdown counts as 1", 10886, perRow, math.NaN(), floor, jsonlIssuesBudgetCeiling, floor},
		{"a floor above the ceiling gives the ceiling", 10886, perRow, 1, 20 * time.Minute, jsonlIssuesBudgetCeiling, jsonlIssuesBudgetCeiling},
		{"a slowdown of 0.2 counts as 1 above a zero floor", 10886, perRow, 0.2, 0, jsonlIssuesBudgetCeiling, 3265800 * time.Microsecond},
		{"a NaN slowdown counts as 1 above a zero floor", 10886, perRow, math.NaN(), 0, jsonlIssuesBudgetCeiling, 3265800 * time.Microsecond},
		{"an estimate too large for a Duration is the ceiling", math.MaxInt32, time.Hour, 1e9, floor, jsonlIssuesBudgetCeiling, jsonlIssuesBudgetCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := jsonlTableBudget(tc.rows, tc.perRow, tc.slowdown, tc.floor, tc.ceiling)
			if got != tc.want {
				t.Errorf("jsonlTableBudget(%d, %v, %v, %v, %v) = %v, want %v",
					tc.rows, tc.perRow, tc.slowdown, tc.floor, tc.ceiling, got, tc.want)
			}
		})
	}
}
