package done

import (
	"reflect"
	"testing"
)

func TestLineLimitVerdict(t *testing.T) {
	t.Parallel()
	refusal := []string{
		"Change exceeds the 70-added-line constraint: 141 added lines against 70 declared (the refusal is at twice the limit).",
		splitLine,
	}
	tests := []struct {
		name   string
		limits []int
		added  int
		refuse bool
		want   []string
	}{
		{"no limit", nil, 500, false, []string{noLimitLine}},
		{"twice the limit passes", []int{70}, 140, false, []string{
			"Change exceeds the 70-added-line constraint: 140 added lines against 70 declared (the refusal is at twice the limit)."}},
		{"past twice refuses", []int{70}, 141, true, refusal},
		{"two numbers read at the larger", []int{45, 60}, 121, true, []string{
			"Change exceeds the 60-added-line constraint: 121 added lines against 60 declared (the bead states 45 and 60; the refusal is at twice the limit).", splitLine}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if refuse, got := lineLimitVerdict(tc.limits, tc.added); refuse != tc.refuse || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("lineLimitVerdict(%v, %d): refuse %v, lines %q; want refuse %v, lines %q",
					tc.limits, tc.added, refuse, got, tc.refuse, tc.want)
			}
		})
	}
}
