package specdispatch

import (
	"strings"
	"testing"
)

// TestSpecLintEstimateRequiredOffByDefault: the zero LintOptions reads no
// estimate requirement, so the dispatcher's candidate lint and every other
// library caller keep the shape check they always made (gt-1cx0o0).
func TestSpecLintEstimateRequiredOffByDefault(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	if v := LintWith(s, defaultTemplate(), LintOptions{}); !v.Clean() {
		t.Fatalf("the zero options refused a clean bead with no sp-minutes label: %s", v.Line(s.ID))
	}
}

// TestSpecLintEstimateRequiredRefusal: with the requirement on, a work bead
// carrying no sp-minutes label is refused, and the reason names the bead and
// the two commands that write an estimate. A bead whose sp-rung is none or
// whose label does not read is not refused for a missing estimate (gt-1cx0o0).
func TestSpecLintEstimateRequiredRefusal(t *testing.T) {
	t.Parallel()
	t.Run("no label", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, id string }{
			{"with an id", "gt-good"},
			{"without an id", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				s := goodSpec()
				s.ID = tc.id
				wantID := tc.id
				if wantID == "" {
					wantID = "<bead-id>"
				}
				v := LintWith(s, defaultTemplate(), LintOptions{RequireEstimate: true})
				if v.Clean() || v.Field != "estimate" {
					t.Fatalf("verdict = %s (field %q), want an estimate refusal", v.Line(s.ID), v.Field)
				}
				for _, want := range []string{
					"no story-point estimate (no sp-minutes label)",
					"gt spec lint --estimate " + wantID,
					"gt spec estimate " + wantID,
				} {
					if !strings.Contains(v.Reason, want) {
						t.Errorf("reason %q does not name %q", v.Reason, want)
					}
				}
			})
		}
	})
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"the rung is none", []string{"sp-rung:none"}},
		{"the label does not read", []string{"sp-minutes:soon"}},
		{"the label reads", []string{"sp-minutes:8.8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec("one worker, one MR", tc.labels...)
			if v := LintWith(s, defaultTemplate(), LintOptions{RequireEstimate: true}); !v.Clean() {
				t.Fatalf("bead %v refused: %s", tc.labels, v.Line(s.ID))
			}
		})
	}
}
