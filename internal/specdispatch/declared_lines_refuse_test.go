package specdispatch

import (
	"strings"
	"testing"
)

// TestDeclaredLinesNeverRefuse pins gt-82g9zr: a Change line below the estimate
// band's median is advice only. Lint never refuses on it, at any declaration or
// estimate, and the advice line is the one place the author hears about it. A
// bead the band cannot place gets neither the advice nor a refusal.
func TestDeclaredLinesNeverRefuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, size string
		labels     []string
		wantAdvice bool
	}{
		{"30 declared at 8 minutes", workerSizeWithBudget("Change: at most 30 added lines in 2 files."), []string{"sp-minutes:8"}, true},
		{"30 declared at 9.8 minutes", workerSizeWithBudget("Change: at most 30 added lines in 2 files."), []string{"sp-minutes:9.8"}, true},
		{"67 declared at the median at 8 minutes", workerSizeWithBudget("Change: at most 67 added lines in 2 files."), []string{"sp-minutes:8"}, false},
		{"120 declared at 8 minutes", workerSizeWithBudget("Change: at most 120 added lines in 2 files."), []string{"sp-minutes:8"}, false},
		{"no sp-minutes label", workerSizeWithBudget("Change: at most 30 added lines in 2 files."), nil, false},
		{"no Change line", "One worker, one landing.", []string{"sp-minutes:8"}, false},
		{"an unparsable Change line", workerSizeWithBudget("Change: lots of lines."), []string{"sp-minutes:8"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec(tc.size, tc.labels...)
			v := Lint(s, defaultTemplate())
			for _, r := range v.Refusals {
				if r.Field == "size" {
					t.Fatalf("size refusal = %q, want none (verdict %s)", r.Reason, v.Line(s.ID))
				}
			}
			if v.Route == RouteRefuse {
				t.Fatalf("Lint() refused %s, want no refusal", v.Line(s.ID))
			}
			advised := false
			for _, line := range Advice(s) {
				advised = advised || strings.Contains(line, "the Change line declares")
			}
			if advised != tc.wantAdvice {
				t.Fatalf("Advice() declared-lines line = %t, want %t", advised, tc.wantAdvice)
			}
		})
	}
}
