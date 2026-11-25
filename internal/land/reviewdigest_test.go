package land

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// TestVerdictReuseExpectedFlagsAlways pins the flags every review run carries,
// on an input built the way the landing builds one with the conditional inputs
// cleared, so only the always-present flags can appear.
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
			in.Round = 0
			got := ExpectedReviewFlags(in, 0)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReuseExpectedFlags pins the flags that depend on the review's bar
// and round, one row per condition reviewOnce applies: the flag is present when
// its input is set and absent when only the other input is, so a condition
// gated on the wrong field fails its rows.
func TestVerdictReuseExpectedFlags(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	// input builds the landing's input for a bead on its first round, the way
	// the landing builds one; a row then sets only the inputs it names.
	input := func() ReviewInput {
		issue := &beads.Issue{ID: "gt-abc"}
		return BuildReviewInput(issue, nil, op, "", "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
	}
	baseline := []string{"--base", "--bead", "--head", "--out", "-C"}
	with := func(extra ...string) []string {
		want := append(slices.Clone(baseline), extra...)
		slices.Sort(want)
		return want
	}
	for _, tc := range []struct {
		name  string
		round int
		bar   float64
		want  []string
	}{
		{"no conditional input", 0, 0, baseline},
		{"only bar > 0", 0, 0.7, with("--threshold")},
		{"only Round > 0", 1, 0, with("--round")},
		{"bar and Round", 1, 0.7, with("--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input()
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}
