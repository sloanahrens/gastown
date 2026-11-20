package done

import (
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
		{name: "under, including tests", text: "Under 60 added lines including tests", want: []int{60}},
		{name: "hyphenated", text: "a 70-added-line constraint", want: []int{70}},
		{name: "up to", text: "up to 15 added lines", want: []int{15}},
		{name: "roughly, other case", text: "Roughly 25 ADDED LINES", want: []int{25}},
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
