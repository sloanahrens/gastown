package formula

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// A seat runs no check and the landing gate runs them all (gt-13dre6 Q44):
// checkProgram matches a shell-fence check program, checkProse a line that
// tells the seat to run one, and checkVar a rendered *_command variable.
var (
	checkProgram = regexp.MustCompile(`(?m)^[ \t]*((go|make|golangci-lint|gofmt|npm|pnpm|npx|yarn|bats|python3?|tsc|eslint|pytest|cargo|mvn|gradle)\s[^\n]*)`)
	checkProse   = regexp.MustCompile(`(?im)^[ \t]*[-*]?[ \t]*(run|execute)[ \t]+(the |a )?(go|make|golangci-lint|gofmt|npm|pnpm|npx|yarn|bats|python3?|tsc|eslint|pytest|cargo|mvn|gradle|build|test|tests|lint|linter|typecheck|setup)\b`)
	checkVar     = regexp.MustCompile(`\{\{(setup|build|typecheck|lint|test)_command\}\}`)
)

// checkRule is the sentence mol-polecat-work states the rule in once, in its
// intro; every other step points at it rather than restating it (R2).
const checkRule = "never build, lint, vet, test or run anything"

// writeOnlyProblems lists each way mol-polecat-work tells a seat to run a check
// the landing gate owns, restates the rule, or drops a rule it must carry.
func writeOnlyProblems(text string) []string {
	var out []string
	for _, m := range checkProgram.FindAllStringSubmatch(text, -1) {
		out = append(out, "a step runs "+strings.TrimSpace(m[1]))
	}
	for _, m := range checkProse.FindAllString(text, -1) {
		out = append(out, "a step says "+strings.TrimSpace(m))
	}
	for _, m := range checkVar.FindAllString(text, -1) {
		out = append(out, "a step renders "+m)
	}
	for _, want := range []string{
		"the rig's configured pre-submit command, which may check nothing",
		"excerpt of the failing gate log",
		"cannot reproduce it",
		"`gt escalate` instead of guessing",
	} {
		if !strings.Contains(text, want) {
			out = append(out, "the text lacks "+want)
		}
	}
	switch n := strings.Count(text, checkRule); {
	case n == 0:
		out = append(out, "the text lacks "+checkRule)
	case n > 1:
		out = append(out, fmt.Sprintf("the text states the rule %d times", n))
	}
	for _, restated := range []string{
		"submits without checking",
		"lint, build and the tests of the packages",
		"runs none of them",
		"you run none",
		"You run no check of your own",
	} {
		if strings.Contains(text, restated) {
			out = append(out, "the text restates the rule: "+restated)
		}
	}
	return out
}

// TestPolecatFormulaSeatRunsNoCheck pins gt-77m0ei: each row is a mutation
// whose refusal the assertion must name.
func TestPolecatFormulaSeatRunsNoCheck(t *testing.T) {
	t.Parallel()
	text := embeddedFormulaText(t, "mol-polecat-work")
	if got := writeOnlyProblems(text); len(got) != 0 {
		t.Errorf("mol-polecat-work breaks the run-no-check rule:\n%s", strings.Join(got, "\n"))
	}
	for _, tc := range []struct{ name, text, want string }{
		{"a step runs make", text + "\n```bash\nmake build\n```\n", "a step runs make build"},
		{"a step runs golangci-lint", text + "\n```bash\ngolangci-lint run ./...\n```\n", "a step runs golangci-lint run ./..."},
		{"a step renders build_command", text + "\n{{build_command}}\n", "a step renders {{build_command}}"},
		{"a step says to run the tests", text + "\nRun the tests before submitting.\n", "a step says Run the tests"},
		{"the submit rule is gone", strings.Replace(text, "the rig's configured pre-submit command, which may check nothing", "", 1), "lacks the rig's configured pre-submit command, which may check nothing"},
		{"submits without checking is back", text + "\nThe seat submits without checking.\n", "restates the rule: submits without checking"},
		{"the old gate sentence is back", text + "\nIt runs lint, build and the tests of the packages your branch changed.\n", "restates the rule: lint, build and the tests of the packages"},
		{"runs none of them is back", text + "\nThe landing gate runs the checks; a seat runs none of them.\n", "restates the rule: runs none of them"},
		{"you run none is back", text + "\nThe landing gate runs the tests, you run none.\n", "restates the rule: you run none"},
		{"you run no check of your own is back", text + "\nYou run no check of your own.\n", "restates the rule: You run no check of your own"},
		{"the rule is gone", strings.Replace(text, checkRule, "the gate checks", 1), "lacks " + checkRule},
		{"the rule is stated twice", text + "\n" + checkRule + ".\n", "the text states the rule 2 times"},
		{"the excerpt is gone", strings.Replace(text, "excerpt of the failing gate log", "", 1), "lacks excerpt of the failing gate log"},
		{"the reproduction claim is gone", strings.Replace(text, "cannot reproduce it", "", 1), "lacks cannot reproduce it"},
		{"the escalation is gone", strings.Replace(text, "`gt escalate` instead of guessing", "", 1), "lacks `gt escalate` instead of guessing"},
	} {
		if got := strings.Join(writeOnlyProblems(tc.text), "\n"); !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q do not name %q", tc.name, got, tc.want)
		}
	}
}
