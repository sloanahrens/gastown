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

// TestDeclaredLinesAdvice pins the advisory: a bead declaring fewer added lines
// than its band advises gets one line naming the band's numbers and the
// suggested minimum; a declaration at Advise, a missing label, a missing or
// unparsable Change line, and minutes outside the table get none. A
// declaration below Refuse is refused instead, so it gets no advice line
// (gt-vb9pi9); 38 is the band's p25 at 8 minutes.
func TestDeclaredLinesAdvice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		size   string
		labels []string
		want   string
	}{
		{"38 declared at 8 minutes", "Change: at most 38 added lines.", []string{"sp-minutes:8"},
			"the Change line declares 38 added lines, but beads estimated at 8.0 minutes landed with a median of 67 (p75 119, p90 236) and the gate refuses past twice the declared number: declare at least 118, or split"},
		{"35 declared at 8 minutes", "Change: at most 35 added lines.", []string{"sp-minutes:8"}, ""},
		{"120 declared at 8 minutes", "Change: at most 120 added lines.", []string{"sp-minutes:8"}, ""},
		{"no label", "Change: at most 35 added lines.", nil, ""},
		{"no change line", "one worker, one landing.", []string{"sp-minutes:8"}, ""},
		{"unparsable change line", "Change: lots of lines.", []string{"sp-minutes:8"}, ""},
		{"minutes outside the table", "Change: at most 35 added lines.", []string{"sp-minutes:17"}, ""},
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
