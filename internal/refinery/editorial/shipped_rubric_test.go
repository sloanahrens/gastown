package editorial

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// shippedRubricPath is the path of the rubric this repository deploys at its
// root. Nothing else in the build reads it: the container gate runs the
// tests, docs-lint reads prose, and DiffRubricAt only compares a rubric
// against a later revision of itself — so a criterion lost to a careless
// edit, or to a merge that resolved the array by taking one side, is
// invisible to every other gate.
func shippedRubricPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the shipped rubric")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".om.json")
}

// shippedRubricCriteria parses the shipped rubric with the same parser the
// gate uses, so a file this test accepts is one the review path can read.
func shippedRubricCriteria(t *testing.T) []RubricCriterion {
	t.Helper()
	path := shippedRubricPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped rubric %s: %v", path, err)
	}
	criteria, err := ParseRubricCriteria(data)
	if err != nil {
		t.Fatalf("parse the shipped rubric %s: %v", path, err)
	}
	return criteria
}

// TestShippedRubricIsGradeable asserts every criterion the town deploys
// carries what a reviewer needs to grade it: a name to report, a weight to
// move the score, and guidance to grade against. A criterion with an empty
// guidance asks the reviewer for nothing while still diluting the weights of
// the criteria that do ask for something.
func TestShippedRubricIsGradeable(t *testing.T) {
	criteria := shippedRubricCriteria(t)
	if len(criteria) == 0 {
		t.Fatalf("the shipped rubric %s names no criterion, so this repository reviews under om's default rubric instead of its own", shippedRubricPath(t))
	}
	for i, c := range criteria {
		if strings.TrimSpace(c.Name) == "" {
			t.Errorf("criterion %d has no name: %+v", i, c)
		}
		if c.Weight <= 0 {
			t.Errorf("criterion %q has weight %v, so it cannot move the score", c.Name, c.Weight)
		}
		if strings.TrimSpace(c.Guidance) == "" {
			t.Errorf("criterion %q carries no guidance, so the reviewer has nothing to grade", c.Name)
		}
	}
}

// alarmingBranchCriterion is the criterion gt-07mfu added: a check is not
// accepted unless a test drives its failing branch and shows the alarm. It
// is the test the criterion itself asks for — delete the criterion and this
// fails by name, rather than the town silently reviewing without it.
const alarmingBranchCriterion = "alarming-branch"

// TestShippedRubricCarriesAlarmingBranchCriterion pins the criterion and the
// two things that make it gradeable: the weight that puts it among the
// blocking criteria, and guidance that still names the failing branch. A
// deliberate reword rewords this test in the same commit; a reword that
// leaves the name and drops the requirement is what it is here to catch.
func TestShippedRubricCarriesAlarmingBranchCriterion(t *testing.T) {
	for _, c := range shippedRubricCriteria(t) {
		if c.Name != alarmingBranchCriterion {
			continue
		}
		if c.Weight != 3 {
			t.Errorf("%s weight = %v, want 3: gt-07mfu weights it with the criteria that block on their own", alarmingBranchCriterion, c.Weight)
		}
		if !strings.Contains(c.Guidance, "failing branch") {
			t.Errorf("%s guidance no longer requires a test of the failing branch: %q", alarmingBranchCriterion, c.Guidance)
		}
		return
	}
	t.Fatalf("the shipped rubric %s carries no %q criterion: the class gt-07mfu gated (a check whose failure path is untested, so a broken check reads exactly like a passing one) is unguarded until it is restored", shippedRubricPath(t), alarmingBranchCriterion)
}

// repoContextFile is the document the docs-and-comments criterion grades
// against: the repository's own writing standard.
const repoContextFile = "docs/writing-for-agents.md"

// docsAndCommentsCriterion is the criterion gt-nj23.5 added and gt-9yu4q
// reworded. It arrived citing the standard as the range "R1-R13"; the standard
// gained R14 (gt-jq95) after that line was written, so a reviewer reading the
// range literally left R14's class ungraded exactly where it most often
// appears — a claim written into a doc, which the om gate executes nothing to
// check.
const docsAndCommentsCriterion = "docs-and-comments"

// frozenRuleRange matches a citation naming both ends of a rule range, the
// form that rots the moment the standard gains a rule.
var frozenRuleRange = regexp.MustCompile(`\bR\d+\s*[-–—]\s*R\d+\b`)

// TestShippedRubricCarriesDocsAndCommentsCriterion pins the three things
// gt-9yu4q's reword must keep: the criterion's name, the pointer it grades by
// (R1 — a line that names another document states what it is), and the drift
// requirement that gives the criterion its teeth. A deliberate reword rewords
// this test in the same commit; a reword that leaves the name and drops the
// pointer is what it is here to catch.
func TestShippedRubricCarriesDocsAndCommentsCriterion(t *testing.T) {
	for _, c := range shippedRubricCriteria(t) {
		if c.Name != docsAndCommentsCriterion {
			continue
		}
		if !strings.Contains(c.Guidance, repoContextFile) {
			t.Errorf("%s guidance no longer names %s, so the reviewer has nothing left to read: %q", docsAndCommentsCriterion, repoContextFile, c.Guidance)
		}
		if m := frozenRuleRange.FindString(c.Guidance); m != "" {
			t.Errorf("%s guidance cites the rule set as a frozen range (%q). The range stops at the rules that existed when the line was written, so every rule added later is silently ungraded; cite the rules by pointer to %s instead", docsAndCommentsCriterion, m, repoContextFile)
		}
		if !strings.Contains(c.Guidance, "stale or contradicted") {
			t.Errorf("%s guidance no longer names the drift it grades: %q", docsAndCommentsCriterion, c.Guidance)
		}
		return
	}
	t.Fatalf("the shipped rubric %s carries no %q criterion: the class gt-nj23.5 gated (a stale or contradicted doc or comment that nothing reads) is ungraded until it is restored", shippedRubricPath(t), docsAndCommentsCriterion)
}
