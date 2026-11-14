package storypoints

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specdispatch's story-point size gate repeats this package's sp-minutes
// grammar because this package imports it and it cannot import back. The two
// copies must read a label the same way: a spelling this package accepts is
// judged by the gate at the same minutes, and one this package refuses is named
// unreadable instead of judged. A drift would let the gate pass in silence over
// a bead this package wrote (gt-mtzx6s).
func TestMinutesGrammarMatchesSpecDispatch(t *testing.T) {
	t.Parallel()
	const over = 11
	tmpl := specdispatch.Template{Sections: specdispatch.DefaultSections}
	labels := []string{
		MinutesLabel(12.3), MinutesLabel(8.8), MinutesLabel(0.75),
		"sp-minutes:11",
		"sp-minutes:0", "sp-minutes:-2", "sp-minutes:", "sp-minutes:soon",
		"sp-minutes:NaN", "sp-minutes:+Inf", "sp-minutes:012.5", "sp-minutes:1e3",
	}
	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			spec := specdispatch.Spec{
				ID: "gt-grammar", Title: "A thing", Type: "task", Status: "open",
				Description: "## Goal\nA thing.\n\n## Constraints\nGo only.\n\n## Out of scope\nNothing.\n\n## Gate\nmake gate\n\n## Size\none worker, one MR",
				Acceptance:  "- [ ] one\n- [ ] two\n- [ ] three",
				Labels:      []string{label},
			}
			parsed, err := ParseLabel(label)
			v := specdispatch.LintWith(spec, tmpl, specdispatch.LintOptions{LintOverMinutes: over})
			warned := ""
			for _, w := range v.Warnings {
				if strings.Contains(w, "the size gate did not run") {
					warned = w
				}
			}
			if err != nil {
				if warned == "" {
					t.Fatalf("the gate judged %q, which this package refuses: %s", label, v.Line(spec.ID))
				}
				return
			}
			if warned != "" {
				t.Fatalf("the gate could not read %q, which this package accepts: %s", label, warned)
			}
			if want := parsed.Minutes >= over; want != (v.Route == specdispatch.RouteRefuse) {
				t.Errorf("gate route %q for %q (%v minutes, setting %v), want refused=%v", v.Route, label, parsed.Minutes, over, want)
			}
		})
	}
}
