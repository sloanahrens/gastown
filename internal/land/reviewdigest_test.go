package land

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// TestVerdictReuseExpectedFlagsAlways pins the flags every review run carries,
// on an input built the way the landing builds one.
func TestVerdictReuseExpectedFlagsAlways(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	for _, tc := range []struct {
		name   string
		beadID string
		want   []string
	}{
		{"bead named", "gt-abc", []string{"--base", "--bead", "--head", "--out", "-C"}},
		{"no bead", "", []string{"--base", "--head", "--out", "-C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: tc.beadID}
			in := BuildReviewInput(issue, nil, op, "", "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
			got := ExpectedReviewFlags(in, 0)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}
