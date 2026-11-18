package omlessons

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// proposeGoldenPath is the output `gt land om-lessons --propose` prints for the
// fixture town, which internal/cmd's TestLandOmLessonsProposeGolden pins. The
// test below reads it rather than restating the drafts: the golden is the only
// place a draft appears, so a draft that drifts reddens both tests.
const proposeGoldenPath = "../cmd/testdata/land_om_lessons_propose.golden"

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
	golden := readProposeGolden(t, proposeGoldenPath)

	got := Proposals(report, rules)
	if len(got) != len(report.Classes) {
		t.Fatalf("Proposals() returned %d entries for %d classes", len(got), len(report.Classes))
	}
	for i, p := range got {
		want, ok := golden[p.Name]
		if !ok {
			t.Fatalf("class %q: the golden prints no draft for it", p.Name)
		}
		if p.TooFew {
			// A too-few line carries the class name and the count only.
			if !want.TooFew || want.Current.Findings != p.Current.Findings {
				t.Errorf("class %q = %+v, want the golden's too-few line with %d finding(s)",
					p.Name, p, want.Current.Findings)
			}
			continue
		}
		if want.TooFew {
			t.Errorf("class %q: Proposals drafted it, the golden marks it too few", p.Name)
			continue
		}
		if !reflect.DeepEqual(p, want) {
			t.Errorf("Proposals()[%d] =\n%+v\nwant the golden's draft\n%+v", i, p, want)
		}
	}
}

// readProposeGolden parses the drafts the command printed, keyed by class name.
// A "--- name (id)" heading opens a draft whose Coverage, Counts and Examples
// lines follow; a "too few to say: name (N finding(s) in the window)" line is a
// class under the threshold, carrying only its name and count. A heading
// missing one of its lines, or a line that does not parse, is an error: a
// renamed line must not read as a zero draft.
func readProposeGolden(t *testing.T, path string) map[string]Proposal {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := map[string]Proposal{}
	var cur *Proposal
	var haveCoverage, haveCounts, haveExamples bool
	flush := func() {
		if cur == nil {
			return
		}
		if !haveCoverage || !haveCounts || !haveExamples {
			t.Fatalf("golden draft %q is missing its coverage, counts or examples line", cur.Name)
		}
		out[cur.Name] = *cur
		cur = nil
		haveCoverage, haveCounts, haveExamples = false, false, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			flush()
			name, id, err := splitGoldenHeading(strings.TrimPrefix(line, "--- "))
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			cur = &Proposal{ID: id, Name: name}
		case strings.HasPrefix(line, "too few to say: "):
			flush()
			name, findings, err := splitGoldenTooFew(strings.TrimPrefix(line, "too few to say: "))
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			out[name] = Proposal{Name: name, TooFew: true, Current: ClassCounts{Findings: findings}}
		case strings.HasPrefix(line, "Coverage: ") && cur != nil:
			cur.Coverage = strings.TrimPrefix(line, "Coverage: ")
			haveCoverage = true
		case strings.HasPrefix(line, "Counts: ") && cur != nil:
			if err := splitGoldenCounts(line, &cur.Current, &cur.Prior); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			haveCounts = true
		case strings.HasPrefix(line, "Examples: ") && cur != nil:
			if examples := strings.TrimPrefix(line, "Examples: "); examples != "" {
				cur.Examples = strings.Split(examples, ", ")
			}
			haveExamples = true
		}
	}
	flush()
	if len(out) == 0 {
		t.Fatalf("%s holds no drafts", path)
	}
	return out
}

// splitGoldenHeading splits "an error reads as success (fail-open)" into the
// class name and id.
func splitGoldenHeading(s string) (name, id string, err error) {
	i := strings.LastIndex(s, " (")
	if i < 0 || !strings.HasSuffix(s, ")") {
		return "", "", fmt.Errorf("draft heading %q is not \"name (id)\"", s)
	}
	return s[:i], s[i+2 : len(s)-1], nil
}

// splitGoldenTooFew splits "a secret reaches output (2 finding(s) in the
// window)" into the class name and the finding count.
func splitGoldenTooFew(s string) (name string, findings int, err error) {
	const suffix = " finding(s) in the window)"
	i := strings.LastIndex(s, " (")
	if i < 0 || !strings.HasSuffix(s, suffix) {
		return "", 0, fmt.Errorf("too-few line %q is not \"name (N finding(s) in the window)\"", s)
	}
	findings, err = strconv.Atoi(strings.TrimSuffix(s[i+2:], suffix))
	if err != nil {
		return "", 0, fmt.Errorf("too-few line %q: %w", s, err)
	}
	return s[:i], findings, nil
}

// splitGoldenCounts reads a draft's counts line into the window's counts and
// the prior window's.
func splitGoldenCounts(line string, current, prior *ClassCounts) error {
	format := "Counts: %d finding(s) on %d bead(s), %d round(s) burned; prior window %d finding(s) on %d bead(s), %d round(s)"
	n, err := fmt.Sscanf(line, format, &current.Findings, &current.Beads, &current.Rounds,
		&prior.Findings, &prior.Beads, &prior.Rounds)
	if err != nil || n != 6 {
		return fmt.Errorf("counts line %q does not match %q", line, format)
	}
	return nil
}

// TestReworkProgramProposalDraft: a draft names the coverage, the two windows'
// counts, the example beads and the directive edit, and nothing about which
// caller prints it. The daemon files this text and the command prints it, so
// this is the one place the draft is written down (gt-f5oavm).
func TestReworkProgramProposalDraft(t *testing.T) {
	t.Parallel()
	p := Proposal{
		ID:       "fail-open",
		Name:     "an error reads as success",
		Coverage: "violated-anyway: rule #1 covers it and it still appears",
		Current:  ClassCounts{Findings: 6, Beads: 4, Rounds: 2},
		Prior:    ClassCounts{Findings: 1, Beads: 1},
		Examples: []string{"gt-a", "gt-b", "gt-c"},
	}
	want := `Coverage: violated-anyway: rule #1 covers it and it still appears
Counts: 6 finding(s) on 4 bead(s), 2 round(s) burned; prior window 1 finding(s) on 1 bead(s), 0 round(s)
Examples: gt-a, gt-b, gt-c

Goal: a directive edit that stops om raising this class.
Constraints: the edit goes to the directive files in /town/directives; the edit text is the overseer's to write. A rule that changes what polecats may do needs the operator's yes.
Out of scope: om's bar or threshold; turning anything on.
Gate: go test ./internal/cmd -run TestLandOmLessons -count=1
Size: one directive edit, one landing.
Acceptance:
- [ ] the rule for this class says what the case is and what to do instead
- [ ] the next weekly pass counts fewer findings of this class than this window
`
	if got := ProposalDraft(p, "/town/directives"); got != want {
		t.Errorf("ProposalDraft() =\n%q\nwant\n%q", got, want)
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
