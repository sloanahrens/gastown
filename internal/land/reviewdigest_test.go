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

// TestVerdictReuseExpectedFlagsCriteriaContext pins --criteria-file and
// --context-file, each gated on its own input as reviewOnce gates the argv:
// the criteria flag on the bead's acceptance text and the context flag on the
// accepted-risk record. Each row that has one text and not the other fails if
// the condition reads the wrong field, and the rows without the texts fail if
// one is added unconditionally.
func TestVerdictReuseExpectedFlagsCriteriaContext(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	const criteria = "- [ ] the review tool runs"
	const accepted = "risk: a stale cache is accepted"
	// input builds the landing's input the way the landing builds one, from a
	// bead carrying criteria (or none when empty) and the accepted-risk text the
	// row names; a row then sets only the other inputs it names.
	input := func(criteriaText, acceptedText string) ReviewInput {
		issue := &beads.Issue{ID: "gt-abc", AcceptanceCriteria: criteriaText}
		return BuildReviewInput(issue, nil, op, acceptedText, "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
	}
	baseline := []string{"--base", "--bead", "--head", "--out", "-C"}
	with := func(extra ...string) []string {
		want := append(slices.Clone(baseline), extra...)
		slices.Sort(want)
		return want
	}
	prior := []Finding{{ID: "gt-abc.1", Severity: "major", Path: "internal/land/x.go", Line: 9, Title: "x"}}
	for _, tc := range []struct {
		name     string
		criteria string
		accepted string
		prior    []Finding
		round    int
		bar      float64
		want     []string
	}{
		{"neither text", "", "", nil, 0, 0, baseline},
		{"only the criteria text", criteria, "", nil, 0, 0, with("--criteria-file")},
		{"only the context text", "", accepted, nil, 0, 0, with("--context-file")},
		{"both texts", criteria, accepted, nil, 0, 0, with("--criteria-file", "--context-file")},
		{"only the other inputs", "", "", prior, 1, 0.7, with("--prior-findings", "--round", "--threshold")},
		{"criteria text and the other inputs", criteria, "", prior, 1, 0.7, with("--criteria-file", "--prior-findings", "--round", "--threshold")},
		{"context text and the other inputs", "", accepted, prior, 1, 0.7, with("--context-file", "--prior-findings", "--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input(tc.criteria, tc.accepted)
			in.PriorFindings = tc.prior
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReuseExpectedFlagsPriorFindings pins --prior-findings: the flag is
// present when the input carries the previous round's findings and absent when
// only the other conditional inputs are set, so a condition gated on the wrong
// field fails its rows (gt-qsi4qi).
func TestVerdictReuseExpectedFlagsPriorFindings(t *testing.T) {
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
	prior := []Finding{{ID: "gt-abc.1", Severity: "major", Path: "internal/land/x.go", Line: 9, Title: "x"}}
	for _, tc := range []struct {
		name  string
		prior []Finding
		round int
		bar   float64
		want  []string
	}{
		{"no prior findings, nothing else", nil, 0, 0, baseline},
		{"only prior findings", prior, 0, 0, with("--prior-findings")},
		{"only the other inputs", nil, 1, 0.7, with("--round", "--threshold")},
		{"prior findings and the other inputs", prior, 1, 0.7, with("--prior-findings", "--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input()
			in.PriorFindings = tc.prior
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}
