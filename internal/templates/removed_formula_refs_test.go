package templates

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The four convoy formulas removed in gt-gzhin.5. The role templates are
// agent-facing instructions: telling a crew worker to run a deleted formula
// sends it down a path that now fails.
var removedConvoyFormulas = []string{"code-review", "design", "mol-plan-review", "mol-prd-review"}

func removedFormulaInvocation(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:formula\s+run|sling|--on)\s+` + regexp.QuoteMeta(name) + `\b`)
}

func removedFormulaFile(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(name) + `\.formula\.toml`)
}

// TestRoleTemplatesDoNotInvokeRemovedConvoyFormulas pins the role templates
// against instructions that name a deleted formula (gt-gzhin.5).
func TestRoleTemplatesDoNotInvokeRemovedConvoyFormulas(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(templateFS, "roles")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, e := range entries {
		data, err := templateFS.ReadFile("roles/" + e.Name())
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
		t.Fatalf("role templates reference removed convoy formulas (gt-gzhin.5):\n%s", strings.Join(offenders, "\n"))
	}
}
