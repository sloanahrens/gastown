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
// the two commands that write an estimate. A bead whose sp-rung is none is not
// refused for a missing estimate, but one whose sp-minutes label cannot be read
// — and one carrying the none rung beside such a label — is (gt-1cx0o0,
// gt-uffeik).
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
		want   []string
	}{
		{
			name:   "the label does not read",
			labels: []string{"sp-minutes:soon"},
			want:   []string{`"sp-minutes:soon" is unreadable as sp-minutes`, "gt spec estimate gt-good", "sp-rung:none"},
		},
		{
			name:   "two labels disagree",
			labels: []string{"sp-minutes:12", "sp-minutes:14"},
			want:   []string{"disagree on the bead's minutes", "gt spec estimate gt-good"},
		},
		{
			name:   "an unreadable label beside the none rung",
			labels: []string{"sp-rung:none", "sp-minutes:soon"},
			want:   []string{`"sp-minutes:soon" is unreadable as sp-minutes`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec("one worker, one MR", tc.labels...)
			v := LintWith(s, defaultTemplate(), LintOptions{RequireEstimate: true})
			if v.Clean() || v.Field != "estimate" {
				t.Fatalf("bead %v verdict = %s (field %q), want an estimate refusal", tc.labels, v.Line(s.ID), v.Field)
			}
			for _, want := range tc.want {
				if !strings.Contains(v.Reason, want) {
					t.Errorf("reason %q does not name %q", v.Reason, want)
				}
			}
		})
	}
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"the rung is none", []string{"sp-rung:none"}},
		{"the label reads", []string{"sp-minutes:8.8"}},
		{"two labels agree", []string{"sp-minutes:8.8", "sp-minutes:8.8"}},
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
