package done

import (
	"bytes"
	"reflect"
	"testing"
)

// TestLineLimitDeclaredForms: DeclaredLineLimits reads the worded and
// hyphenated forms a bead states its added-line limit in, and reads no other
// count as one (gt-obyuam).
func TestLineLimitDeclaredForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []int
	}{
		{name: "Change line", text: "Change: at most 50 added lines in 2 files.", want: []int{50}},
		{name: "under, in files", text: "Under 55 added lines in 2 files.", want: []int{55}},
		{name: "about", text: "About 20 added lines.", want: []int{20}},
		{name: "about, with tests", text: "about 90 added lines with tests", want: []int{90}},
		{name: "about 70", text: "about 70 added lines", want: []int{70}},
		{name: "at most 40", text: "at most 40 added lines", want: []int{40}},
		{name: "under, including tests", text: "Under 60 added lines including tests", want: []int{60}},
		{name: "hyphenated", text: "a 70-added-line constraint", want: []int{70}},
		{name: "up to", text: "up to 15 added lines", want: []int{15}},
		{name: "roughly, other case", text: "Roughly 25 ADDED LINES", want: []int{25}},
		{name: "the budget is", text: "The budget is 300 added lines", want: []int{300}},
		{name: "stop at", text: "stop at 100 added lines", want: []int{100}},
		{name: "a limit of", text: "a limit of 50 added lines", want: []int{50}},
		{name: "the cap colon", text: "the cap: 40 added lines", want: []int{40}},
		{name: "limit colon", text: "limit: 90 added lines", want: []int{90}},
		{name: "measured is not a declaration", text: "measured 122 added lines", want: nil},
		{name: "a budget of files is not a line count", text: "the budget is 3 files", want: nil},
		{name: "a minutes count is not a line count", text: "about 10 minutes", want: nil},
		{name: "a count too large for an int", text: "about 99999999999999999999999 added lines", want: nil},
		{name: "empty", text: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DeclaredLineLimits(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DeclaredLineLimits(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestLineLimitTwoNumbers: a text stating two different limits answers both,
// in order of appearance, and a limit stated twice answers once.
func TestLineLimitTwoNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []int
	}{
		{name: "both forms in order", text: "Under 60 added lines including tests, with a 70-added-line constraint.", want: []int{60, 70}},
		{name: "order of appearance, not form", text: "a 70-added-line constraint, then about 90 added lines with tests", want: []int{70, 90}},
		{name: "same number twice", text: "About 20 added lines. Change: at most 20 added lines in 2 files.", want: []int{20}},
		{name: "about and budget", text: "About 120 added lines. The budget is 300 added lines.", want: []int{120, 300}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DeclaredLineLimits(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DeclaredLineLimits(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestLineLimitBudgetSentence: a bead stating both about 120 and a budget of
// 300 added lines is judged at 300, so 261 added lines is not refused and 601
// is (gt-uq5b9u).
func TestLineLimitBudgetSentence(t *testing.T) {
	t.Parallel()
	const desc = "About 120 added lines. The budget is 300 added lines."
	if err := reportLineLimit(new(bytes.Buffer), new(bytes.Buffer), func() (int, error) { return 261, nil }, desc, nil, true); err != nil {
		t.Fatalf("261 added lines against a stated budget of 300 was refused: %v", err)
	}
	if err := reportLineLimit(new(bytes.Buffer), new(bytes.Buffer), func() (int, error) { return 601, nil }, desc, nil, true); err == nil {
		t.Fatal("601 added lines against a stated budget of 300 was not refused")
	}
}
