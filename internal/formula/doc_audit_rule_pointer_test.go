package formula

import (
	"io/fs"
	"regexp"
	"testing"
)

// frozenRuleRange matches a formula that names the writing-for-agents rule set
// as a closed range ("R1-R13", "R1=<n> ... R13=<n>"). The standard grows; a
// frozen range silently drops every later rule from the checklist (gt-fcjia).
var frozenRuleRange = regexp.MustCompile(`R1-R\d+|\.\.\. R\d+=`)

// TestFormulasCiteTheRuleSetByPointer guards every embedded formula against
// pinning the rule range instead of pointing at docs/writing-for-agents.md.
func TestFormulasCiteTheRuleSetByPointer(t *testing.T) {
	t.Parallel()
	paths, err := fs.Glob(formulasFS, "formulas/*.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		content, err := formulasFS.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := frozenRuleRange.Find(content); m != nil {
			t.Errorf("%s pins the rule set as %q; cite docs/writing-for-agents.md by pointer", p, m)
		}
	}
}
