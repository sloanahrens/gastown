package beads

import (
	"reflect"
	"testing"
)

// TestUncheckedCriteriaReadsTheAcceptanceText: the unchecked-box count reads
// AcceptanceText, the field when set and else the description's section.
func TestUncheckedCriteriaReadsTheAcceptanceText(t *testing.T) {
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
			if got := UncheckedCriteria(tc.issue); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UncheckedCriteria = %q, want %q", got, tc.want)
			}
			if got := HasUncheckedCriteria(tc.issue); got != len(tc.want) {
				t.Errorf("HasUncheckedCriteria = %d, want %d", got, len(tc.want))
			}
		})
	}
}
