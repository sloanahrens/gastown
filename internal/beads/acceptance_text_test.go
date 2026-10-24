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

// TestUncheckedAcceptanceText_PrefixedAcceptanceHeading: the lint reads a
// "## Acceptance criteria" section and dispatches the bead, so gt done and the
// landing policy count the same section — a longer heading must not hide
// unticked boxes from the refusal (gt-a4ay1).
func TestUncheckedAcceptanceText_PrefixedAcceptanceHeading(t *testing.T) {
	t.Parallel()
	issue := &Issue{Description: "## Goal\nx\n## Acceptance criteria\n- [ ] a\n- [x] b\n"}
	if got, want := UncheckedAcceptanceText(issue), []string{"- [ ] a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("UncheckedAcceptanceText = %q, want %q", got, want)
	}
	if got, want := AcceptanceText(issue), "- [ ] a\n- [x] b"; got != want {
		t.Errorf("AcceptanceText = %q, want %q", got, want)
	}
}

// TestUncheckedAcceptanceTextMatchesLintItems: a box is a criterion when the
// spec lint counts it as one, so every list marker the lint accepts counts
// here, and a fenced block is example text that neither counts.
func TestUncheckedAcceptanceTextMatchesLintItems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"star bullet", "* [ ] a", []string{"* [ ] a"}},
		{"plus bullet", "+ [ ] a", []string{"+ [ ] a"}},
		{"numbered dot", "1. [ ] a", []string{"1. [ ] a"}},
		{"numbered paren", "1) [ ] a", []string{"1) [ ] a"}},
		{"mixed markers all count", "* [ ] a\n1. [ ] b", []string{"* [ ] a", "1. [ ] b"}},
		{"checked stays uncounted", "* [x] a\n1. [X] b\n- [x] c", nil},
		{"fenced sample is not a criterion", "- [x] a\n```\n- [ ] sample\n```", nil},
		{"tilde fence is not a criterion", "~~~\n* [ ] sample\n~~~\n- [ ] b", []string{"- [ ] b"}},
		{"box after a fence still counts", "```\n- [ ] sample\n```\n* [ ] b", []string{"* [ ] b"}},
		{"unclosed fence does not hide a later box", "- [x] a\n```\n- [ ] b", []string{"- [ ] b"}},
		{"unclosed tilde fence does not hide a later box", "~~~\n* [ ] b", []string{"* [ ] b"}},
		{"a fence closed by a longer run", "````\n- [ ] sample\n````\n- [ ] b", []string{"- [ ] b"}},
		{"a shorter run does not close a longer fence", "````\n- [ ] sample\n```\n- [ ] sample\n````\n- [ ] b", []string{"- [ ] b"}},
		{"a fence with an info string", "```go\n- [ ] sample\n```\n- [ ] b", []string{"- [ ] b"}},
		{"an indented fence", "  ```\n- [ ] sample\n  ```\n- [ ] b", []string{"- [ ] b"}},
		{"unclosed fence after a closed one", "```\n- [ ] sample\n```\n```\n- [ ] b", []string{"- [ ] b"}},
		{"nested unchecked item counts", "- [x] a\n  * [ ] b", []string{"* [ ] b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := uncheckedBoxes(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("uncheckedBoxes(%q) = %q, want %q", tc.text, got, tc.want)
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
		{
			name:  "a longer acceptance heading is read",
			issue: &Issue{Description: "## Acceptance criteria\n\n- [ ] one\n"},
			want:  "- [ ] one",
		},
		{
			name:  "the exact heading wins over a longer one",
			issue: &Issue{Description: "## Acceptance notes\n\n- [ ] notes\n\n## Acceptance\n\n- [ ] exact\n"},
			want:  "- [ ] exact",
		},
		{
			name:  "two longer headings resolve by name, not position",
			issue: &Issue{Description: "## Acceptance notes\n\n- [ ] notes\n\n## Acceptance criteria\n\n- [ ] criteria\n"},
			want:  "- [ ] criteria",
		},
		{name: "a similarly named heading is not the section", issue: &Issue{Description: "## Accepted trade-offs\n\n- [ ] x\n"}, want: ""},
		{
			name:  "the first of two repeated acceptance headings wins",
			issue: &Issue{Description: "## Acceptance\n\n- [ ] first\n\n## Acceptance\n\n- [ ] second\n"},
			want:  "- [ ] first",
		},
		{
			name:  "a fenced acceptance heading is example text",
			issue: &Issue{Description: "## Goal\n\n```\n## Acceptance\n- [ ] example\n```\n\n## Acceptance\n\n- [ ] real\n"},
			want:  "- [ ] real",
		},
		{
			name:  "a fenced acceptance heading alone is no section",
			issue: &Issue{Description: "## Goal\n\n```\n## Acceptance\n- [ ] example\n```\n"},
			want:  "",
		},
		{
			name:  "a fenced heading inside the section does not end it",
			issue: &Issue{Description: "## Acceptance\n\n```\n## Gate\n```\n- [ ] one\n\n## Out of scope\n\nx\n"},
			want:  "```\n## Gate\n```\n- [ ] one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AcceptanceText(tc.issue); got != tc.want {
				t.Errorf("AcceptanceText = %q, want %q", got, tc.want)
			}
		})
	}
}
