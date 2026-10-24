package beads

import (
	"reflect"
	"testing"
)

// TestUncheckedAcceptanceTextReadsTheAcceptanceText: the refusal gates count
// boxes in the acceptance text, the field when set and else the description's
// section.
func TestUncheckedAcceptanceTextReadsTheAcceptanceText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue *Issue
		want  []string
	}{
		{"nil issue", nil, nil},
		{"field wins over the section", &Issue{AcceptanceCriteria: "- [x] a", Description: "## Acceptance\n- [ ] b"}, nil},
		{"field boxes", &Issue{AcceptanceCriteria: "- [x] a\n  - [ ] b"}, []string{"- [ ] b"}},
		{"description section", &Issue{Description: "## Goal\n- [ ] not here\n\n## Acceptance\n- [x] a\n- [ ] b\n\n## Gate\n- [ ] not here"}, []string{"- [ ] b"}},
		{"no section", &Issue{Description: "## Goal\n- [ ] a"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := UncheckedAcceptanceText(tc.issue); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UncheckedAcceptanceText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUncheckedCriteriaReadsTheFieldOnly: the close gates share
// UncheckedCriteria, and the description's section does not widen them: a bead
// whose only boxes sit in that section keeps closing as it did before
// gt-1mrji (gt-zep4x).
func TestUncheckedCriteriaReadsTheFieldOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		issue *Issue
		want  []string
	}{
		{"nil issue", nil, nil},
		{"field boxes", &Issue{AcceptanceCriteria: "- [x] a\n  - [ ] b"}, []string{"- [ ] b"}},
		{"fill the field, ignore the section", &Issue{AcceptanceCriteria: "- [x] a", Description: "## Acceptance\n- [ ] b"}, nil},
		{"section boxes are not criteria here", &Issue{Description: "## Acceptance\n- [ ] b"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := UncheckedCriteria(tc.issue); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UncheckedCriteria = %q, want %q", got, tc.want)
			}
			if got := HasUncheckedCriteria(tc.issue); got != len(tc.want) {
				t.Errorf("HasUncheckedCriteria = %d, want %d", got, len(tc.want))
			}
		})
	}
}

// TestAcceptanceTextReadsTheFieldThenTheDescriptionSection: the acceptance
// field wins when set, and the description's "## Acceptance" body is the text
// otherwise — every item, checked and unchecked alike.
func TestAcceptanceTextReadsTheFieldThenTheDescriptionSection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		issue *Issue
		want  string
	}{
		{name: "nil issue", issue: nil, want: ""},
		{
			name:  "the field wins over the section",
			issue: &Issue{AcceptanceCriteria: "- [ ] from the field", Description: "## Acceptance\n\n- [ ] from the description\n"},
			want:  "- [ ] from the field",
		},
		{
			name:  "a middle section stops at the next heading",
			issue: &Issue{Description: "## Goal\n\nadd b\n\n## Acceptance\n\n- [x] one\n- [ ] two\n\n## Out of scope\n\nnothing\n"},
			want:  "- [x] one\n- [ ] two",
		},
		{
			name:  "a last section runs to the end",
			issue: &Issue{Description: "## Goal\n\nadd b\n\n## Acceptance\n\n- [ ] only one\n"},
			want:  "- [ ] only one",
		},
		{
			name:  "a deeper heading is body text",
			issue: &Issue{Description: "## Acceptance\n\n### the seam\n\n- [ ] one\n"},
			want:  "### the seam\n\n- [ ] one",
		},
		{name: "no section at all", issue: &Issue{Description: "## Goal\n\nadd b\n"}, want: ""},
		{name: "an empty section", issue: &Issue{Description: "## Goal\n\nadd b\n\n## Acceptance\n\n## Out of scope\n\nx\n"}, want: ""},
		{name: "a similarly named heading is not the section", issue: &Issue{Description: "## Accepted trade-offs\n\n- [ ] x\n"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AcceptanceText(tc.issue); got != tc.want {
				t.Errorf("AcceptanceText = %q, want %q", got, tc.want)
			}
		})
	}
}
