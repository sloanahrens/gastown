package formula

import (
	"regexp"
	"strings"
	"testing"
)

// A seat runs no check and the landing gate runs them all (gt-13dre6 Q44):
// checkProgram matches a shell-fence check program, checkVar a *_command var.
var (
	checkProgram = regexp.MustCompile(`(?m)^[ \t]*((go|make|npm|pnpm|npx|tsc|eslint|pytest|cargo|mvn|gradle)\s[^\n]*)`)
	checkProse   = regexp.MustCompile(`(?im)^[ \t]*[-*]?[ \t]*(run|execute)[ \t]+(the |a )?(go|make|npm|pnpm|npx|tsc|eslint|pytest|cargo|mvn|gradle|build|test|tests|lint|linter|typecheck|setup)\b`)
	checkVar     = regexp.MustCompile(`\{\{(setup|build|typecheck|lint|test)_command\}\}`)
)

// writeOnlyProblems lists each way mol-polecat-work tells a seat to run a check the landing gate owns, or omits a rule.
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
	for _, want := range []string{"submits without checking", "excerpt of the failing gate log", "cannot reproduce it", "`gt escalate` instead of guessing"} {
		if !strings.Contains(text, want) {
			out = append(out, "the text lacks "+want)
		}
	}
	return out
}

// TestPolecatFormulaSeatRunsNoCheck pins gt-77m0ei: each row is a mutation whose refusal the assertion must name.
func TestPolecatFormulaSeatRunsNoCheck(t *testing.T) {
	t.Parallel()
	text := embeddedFormulaText(t, "mol-polecat-work")
	if got := writeOnlyProblems(text); len(got) != 0 {
		t.Errorf("mol-polecat-work tells the seat to run a check:\n%s", strings.Join(got, "\n"))
	}
	for _, tc := range []struct{ name, text, want string }{
		{"a step runs make", text + "\n```bash\nmake build\n```\n", "a step runs make build"},
		{"a step renders build_command", text + "\n{{build_command}}\n", "a step renders {{build_command}}"},
		{"a step says to run the tests", text + "\nRun the tests before submitting.\n", "a step says Run the tests"},
		{"the submit rule is gone", strings.Replace(text, "submits without checking", "", 1), "lacks submits without checking"},
		{"the excerpt is gone", strings.Replace(text, "excerpt of the failing gate log", "", 1), "lacks excerpt of the failing gate log"},
		{"the reproduction claim is gone", strings.Replace(text, "cannot reproduce it", "", 1), "lacks cannot reproduce it"},
		{"the escalation is gone", strings.Replace(text, "`gt escalate` instead of guessing", "", 1), "lacks `gt escalate` instead of guessing"},
	} {
		if got := strings.Join(writeOnlyProblems(tc.text), "\n"); !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q do not name %q", tc.name, got, tc.want)
		}
	}
}
