package omlessons

import (
	"reflect"
	"testing"
)

// TestReworkProgramProposalsMatchPrintedDrafts pins Proposals against the
// drafts the command printed for the same fixture: fail-open is covered by rule
// 1, the refusal-branch class names rule 3 which the directive lacks, the
// wall-clock class has no rule, and the secret class is too few to draft.
func TestReworkProgramProposalsMatchPrintedDrafts(t *testing.T) {
	t.Parallel()
	report := Report{
		Days: DefaultDays,
		Classes: []ClassRow{
			{ID: "fail-open", Name: "an error reads as success", Rule: 1,
				Current:      ClassCounts{Findings: 6, Beads: 4, Rounds: 2},
				Prior:        ClassCounts{Findings: 1, Beads: 1},
				ExampleBeads: []string{"gt-a", "gt-b", "gt-c"}},
			{ID: "missing-refusal-test", Name: "a refusal branch has no test", Rule: 3,
				Current:      ClassCounts{Findings: 5, Beads: 5},
				ExampleBeads: []string{"gt-j", "gt-k", "gt-l"}},
			{ID: "wall-clock-wait", Name: "a wait or sleep on the wall clock",
				Current:      ClassCounts{Findings: 5, Beads: 5},
				ExampleBeads: []string{"gt-e", "gt-f", "gt-g"}},
			{ID: "secret-in-output", Name: "a secret reaches output",
				Current:      ClassCounts{Findings: 2, Beads: 2},
				ExampleBeads: []string{"gt-o", "gt-p"}},
		},
	}
	rules := map[string]bool{"1": true, "2": true, "7": true}

	want := []Proposal{
		{ID: "fail-open", Name: "an error reads as success",
			Coverage: "violated-anyway: rule #1 covers it and it still appears",
			Current:  ClassCounts{Findings: 6, Beads: 4, Rounds: 2},
			Prior:    ClassCounts{Findings: 1, Beads: 1},
			Examples: []string{"gt-a", "gt-b", "gt-c"}},
		{ID: "missing-refusal-test", Name: "a refusal branch has no test",
			Coverage: "gap: the table names rule #3 and the directive has no rule #3",
			Current:  ClassCounts{Findings: 5, Beads: 5},
			Examples: []string{"gt-j", "gt-k", "gt-l"}},
		{ID: "wall-clock-wait", Name: "a wait or sleep on the wall clock",
			Coverage: "gap: no numbered rule covers it",
			Current:  ClassCounts{Findings: 5, Beads: 5},
			Examples: []string{"gt-e", "gt-f", "gt-g"}},
		{ID: "secret-in-output", Name: "a secret reaches output", TooFew: true,
			Current:  ClassCounts{Findings: 2, Beads: 2},
			Examples: []string{"gt-o", "gt-p"}},
	}

	if got := Proposals(report, rules); !reflect.DeepEqual(got, want) {
		t.Errorf("Proposals() =\n%+v\nwant\n%+v", got, want)
	}
}

// TestReworkProgramProposalCoverage: a class with enough findings names its
// coverage against the rules the directive carries. Each row is one way a class
// can be covered or a gap.
func TestReworkProgramProposalCoverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rule  int
		rules map[string]bool
		want  string
	}{
		{
			name:  "no numbered rule",
			rule:  0,
			rules: map[string]bool{"1": true},
			want:  "gap: no numbered rule covers it",
		},
		{
			name:  "rule absent from the directive",
			rule:  3,
			rules: map[string]bool{"1": true},
			want:  "gap: the table names rule #3 and the directive has no rule #3",
		},
		{
			name:  "no rules read reads as a gap",
			rule:  1,
			rules: nil,
			want:  "gap: the table names rule #1 and the directive has no rule #1",
		},
		{
			name:  "rule in the directive",
			rule:  1,
			rules: map[string]bool{"1": true},
			want:  "violated-anyway: rule #1 covers it and it still appears",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := Report{Classes: []ClassRow{{ID: "c", Name: "c", Rule: tc.rule,
				Current: ClassCounts{Findings: ProposeMinFindings}}}}
			got := Proposals(report, tc.rules)
			if len(got) != 1 || got[0].Coverage != tc.want {
				t.Errorf("Coverage = %+v, want %q", got, tc.want)
			}
		})
	}
}

// TestReworkProgramProposalsTooFew: the threshold is ProposeMinFindings; a class
// one short of it is too few to draft, and a class at it gets a draft.
func TestReworkProgramProposalsTooFew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		findings int
		wantFew  bool
	}{
		{findings: ProposeMinFindings - 1, wantFew: true},
		{findings: ProposeMinFindings, wantFew: false},
	}
	for _, tc := range tests {
		report := Report{Classes: []ClassRow{{ID: "c", Name: "c", Rule: 1,
			Current: ClassCounts{Findings: tc.findings}}}}
		got := Proposals(report, map[string]bool{"1": true})
		if len(got) != 1 || got[0].TooFew != tc.wantFew {
			t.Errorf("%d finding(s): TooFew = %+v, want %v", tc.findings, got, tc.wantFew)
		}
	}
}
