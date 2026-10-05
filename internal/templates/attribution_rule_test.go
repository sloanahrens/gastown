package templates

import (
	"strings"
	"testing"
)

// Claude Code appends a Co-Authored-By trailer unless told not to, and landing
// refuses a branch that carries one (gt-v4ssj.10). Both texts a polecat reads
// while writing commits must say so in the literal words the bead asked for.
const noAttributionRule = "NO\nCo-Authored-By trailer, no AI attribution anywhere."

func TestPolecatTemplatesForbidAIAttribution(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	role, err := tmpl.RenderRole("polecat", RoleData{
		Role: "polecat", RigName: "myrig", TownRoot: "/test/town", TownName: "town",
		WorkDir: "/test/town/myrig/polecats/TestCat", DefaultBranch: "main", Polecat: "TestCat",
	})
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}

	for name, text := range map[string]string{
		"polecat role template": role,
		"polecat-CLAUDE.md":     polecatCLAUDEmd,
	} {
		if !strings.Contains(text, noAttributionRule) {
			t.Errorf("%s lacks the rule %q", name, noAttributionRule)
		}
	}
}
