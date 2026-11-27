package specdispatch

import "testing"

// declaredLinesSpec is a bead whose Size section is size and whose labels are
// labels, carrying no other advisory trigger.
func declaredLinesSpec(size string, labels ...string) Spec {
	return Spec{
		ID:          "gt-lines",
		Type:        "task",
		Title:       "declared lines fixture",
		Description: "## Goal\nAdd a column.\n\n## Constraints\nGo.\n\n## Size\n" + size,
		Acceptance:  "- [ ] the column renders",
		Labels:      labels,
	}
}

// TestDeclaredLinesAdvice pins the advisory (gt-82g9zr): a bead declaring fewer
// added lines than its band's median gets one line naming the median and the
// refusal gt done applies, twice the median; a declaration at or above the
// median, a missing label, a missing or unparsable Change line, and minutes
// outside the table get none. The median is 109 at 9.8 minutes and 67 at 8.
func TestDeclaredLinesAdvice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		size   string
		labels []string
		want   string
	}{
		{"30 declared at 9.8 minutes", "Change: at most 30 added lines.", []string{"sp-minutes:9.8"},
			"the Change line declares 30 added lines; beads estimated at 9.8 minutes landed with a median of 109: this is advice, declare the lines you count; gt done refuses past 218 added lines"},
		{"35 declared at 8 minutes", "Change: at most 35 added lines.", []string{"sp-minutes:8"},
			"the Change line declares 35 added lines; beads estimated at 8.0 minutes landed with a median of 67: this is advice, declare the lines you count; gt done refuses past 134 added lines"},
		{"67 declared at the median at 8 minutes", "Change: at most 67 added lines.", []string{"sp-minutes:8"}, ""},
		{"109 declared at the median at 9.8 minutes", "Change: at most 109 added lines.", []string{"sp-minutes:9.8"}, ""},
		{"227 declared at 9.8 minutes", "Change: at most 227 added lines.", []string{"sp-minutes:9.8"}, ""},
		{"no label", "Change: at most 30 added lines.", nil, ""},
		{"no change line", "one worker, one landing.", []string{"sp-minutes:9.8"}, ""},
		{"unparsable change line", "Change: lots of lines.", []string{"sp-minutes:9.8"}, ""},
		{"minutes outside the table", "Change: at most 30 added lines.", []string{"sp-minutes:17"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Advice(declaredLinesSpec(tc.size, tc.labels...))
			if tc.want == "" && len(got) != 0 {
				t.Fatalf("Advice() = %q, want none", got)
			}
			if tc.want != "" && (len(got) != 1 || got[0] != tc.want) {
				t.Fatalf("Advice() = %q, want %q", got, tc.want)
			}
		})
	}
}
