package specdispatch

import (
	"strings"
	"testing"
)

// TestLintFollowupBudget pins the follow-up budget refusal (gt-hixqtv): an
// om-followup bead whose Change line declares more added lines or more files
// than a follow-up bead may spend is refused, told to cut the bead by finding
// or by file and not to declare more. A declaration within the budget, a bead
// without the label, and a rig's configured budget larger than the default are
// not refused. The refusal is asked before the declared-lines floor, so a bead
// over the budget is never told to declare more.
func TestLintFollowupBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		size    string
		labels  []string
		opts    LintOptions
		wantCut bool
	}{
		{"120 added lines", workerSizeWithBudget("Change: at most 120 added lines in 2 files."), []string{"om-followup"}, LintOptions{}, true},
		{"4 files", workerSizeWithBudget("Change: at most 40 added lines in 4 files."), []string{"om-followup"}, LintOptions{}, true},
		{"80 lines in 3 files", workerSizeWithBudget("Change: at most 80 added lines in 3 files."), []string{"om-followup"}, LintOptions{}, false},
		{"no om-followup label", workerSizeWithBudget("Change: at most 120 added lines in 2 files."), nil, LintOptions{}, false},
		{"configured 150 lines", workerSizeWithBudget("Change: at most 120 added lines in 2 files."), []string{"om-followup"}, LintOptions{FollowupMaxAddedLines: 150}, false},
		{"configured 5 files", workerSizeWithBudget("Change: at most 40 added lines in 4 files."), []string{"om-followup"}, LintOptions{FollowupMaxFiles: 5}, false},
		{"over budget below the declared-lines floor", workerSizeWithBudget("Change: at most 100 added lines in 2 files."), []string{"om-followup", "sp-minutes:12"}, LintOptions{}, true},
		{"no Change line", "One worker, one landing.", []string{"om-followup"}, LintOptions{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec(tc.size, tc.labels...)
			v := LintWith(s, defaultTemplate(), tc.opts)
			var sizeReasons []string
			for _, r := range v.Refusals {
				if r.Field == "size" {
					sizeReasons = append(sizeReasons, r.Reason)
				}
			}
			cut := false
			for _, reason := range sizeReasons {
				if strings.Contains(reason, "cut the bead by finding or by file") {
					cut = true
				}
				if strings.Contains(reason, "declare at least") {
					t.Errorf("an om-followup bead was told to declare more: %q", reason)
				}
			}
			if cut != tc.wantCut {
				t.Fatalf("cut refusal = %v, want %v; size refusals = %q (verdict %s)", cut, tc.wantCut, sizeReasons, v.Line(s.ID))
			}
			if !tc.wantCut && !v.Clean() {
				t.Fatalf("spec with no cut refusal is not clean: %s", v.Line(s.ID))
			}
		})
	}
}
