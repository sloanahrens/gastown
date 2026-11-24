package specdispatch

import (
	"strings"
	"testing"
)

// TestDeclaredLinesRefuse pins the refusal (gt-vb9pi9): a Change line below
// the estimate band's median (67 at 8 minutes) is refused with the declared
// number, the estimate, the median and the declaration that keeps under the
// gate; a declaration at the median is not refused but keeps the advice, and a
// high declaration or one the band cannot place is neither. A refused bead's
// advice does not repeat the refusal.
func TestDeclaredLinesRefuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, size string
		labels     []string
		wantReason string
		wantAdvice bool
	}{
		{"30 declared at 8 minutes", workerSizeWithBudget("Change: at most 30 added lines in 2 files."), []string{"sp-minutes:8"},
			"the Change line declares 30 added lines, below what half of the beads estimated at 8.0 minutes landed with (median 67); declare at least 118, or split", false},
		{"67 declared gets the advice", workerSizeWithBudget("Change: at most 67 added lines in 2 files."), []string{"sp-minutes:8"}, "", true},
		{"120 declared is neither", workerSizeWithBudget("Change: at most 120 added lines in 2 files."), []string{"sp-minutes:8"}, "", false},
		{"no sp-minutes label", workerSizeWithBudget("Change: at most 30 added lines in 2 files."), nil, "", false},
		{"no Change line", "One worker, one landing.", []string{"sp-minutes:8"}, "", false},
		{"an unparsable Change line", workerSizeWithBudget("Change: lots of lines."), []string{"sp-minutes:8"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec(tc.size, tc.labels...)
			v := Lint(s, defaultTemplate())
			got := ""
			for _, r := range v.Refusals {
				if r.Field == "size" {
					got = r.Reason
				}
			}
			if got != tc.wantReason || (tc.wantReason == "" && v.Route == RouteRefuse) {
				t.Fatalf("size refusal = %q, want %q (verdict %s)", got, tc.wantReason, v.Line(s.ID))
			}
			advice := Advice(s)
			advised := false
			for _, line := range advice {
				advised = advised || strings.Contains(line, "the Change line declares")
			}
			if advised != tc.wantAdvice {
				t.Fatalf("Advice() = %q, want declared-lines advice %t", advice, tc.wantAdvice)
			}
		})
	}
}
