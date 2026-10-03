package formula

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The four convoy formulas removed in gt-gzhin.5. gt formula run no longer
// accepts a convoy formula, so a surviving formula that invokes one produces
// steps whose command fails at runtime.
var removedConvoyFormulas = []string{"code-review", "design", "mol-plan-review", "mol-prd-review"}

// removedFormulaInvocation matches `gt formula run <name>`, `gt sling <name>`
// and `--on <name>`. The name is a whole token, so a survivor whose own name
// ends in a removed name (mol-polecat-code-review) is not flagged.
func removedFormulaInvocation(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:formula\s+run|sling|--on)\s+` + regexp.QuoteMeta(name) + `\b`)
}

// removedFormulaFile matches a reference to the formula's file. The leading
// boundary keeps mol-polecat-code-review.formula.toml from matching code-review.
func removedFormulaFile(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(name) + `\.formula\.toml`)
}

// TestEmbeddedFormulasDoNotInvokeRemovedConvoyFormulas pins the survivors: a
// formula shipped in the binary must not invoke or name a formula gt-gzhin.5
// deleted, or gt will hand an agent a step that cannot run.
func TestEmbeddedFormulasDoNotInvokeRemovedConvoyFormulas(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(formulasFS, "formulas")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, e := range entries {
		data, err := formulasFS.ReadFile("formulas/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, name := range removedConvoyFormulas {
			if m := removedFormulaInvocation(name).FindString(text); m != "" {
				offenders = append(offenders, e.Name()+": invokes removed formula ("+strings.TrimSpace(m)+")")
			}
			if m := removedFormulaFile(name).FindString(text); m != "" {
				offenders = append(offenders, e.Name()+": names removed formula file ("+strings.TrimSpace(m)+")")
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("embedded formulas reference removed convoy formulas (gt-gzhin.5):\n%s", strings.Join(offenders, "\n"))
	}
}
