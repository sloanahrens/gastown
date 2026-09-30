package formula

import (
	"strings"
	"testing"
)

// polecatFormulaDoneWaitVerbatim is the instruction the polecat workflow
// carries about a running `gt done` (gt-7dxw): it runs the local gate itself,
// which takes minutes, and the polecat leaves it alone. A polecat once misread
// the wait as a hang and bd-closed its bead mid-`gt done` (overseer
// hq-wisp-6q5ib). Since D2 (ADR 0004) gt done runs only the unit tier and
// needs no container slot, so there is no slot wait to describe.
const polecatFormulaDoneWaitVerbatim = "gt done runs the local gate itself (lint, build and the tests of the packages your " +
	"branch changed; no container slot), which can take several minutes. That is normal. Do not interrupt it " +
	"and do not close the bead."

// TestPolecatFormulasCarryTheSlotLoopRule guards the three polecat workflows
// that run `gt done`: the default, the monorepo/fork variant, and the doc
// audit (a hand-maintained copy of the default's step text). All three must
// carry the no-retry-loop rule; only the default carries it in full, and the
// variants point to docs/reference.md (gt-7dxw review, R2).
func TestPolecatFormulasCarryTheSlotLoopRule(t *testing.T) {
	for _, name := range []string{
		"mol-polecat-work",
		"mol-polecat-work-monorepo",
		"mol-doc-audit",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := GetEmbeddedFormulaContent(name)
			if err != nil {
				t.Fatalf("GetEmbeddedFormulaContent(%q): %v", name, err)
			}
			text := string(raw)
			wants := []string{
				"Never script a retry around gt done",
				"docs/reference.md",
			}
			if name == "mol-polecat-work" {
				wants = append(wants,
					polecatFormulaDoneWaitVerbatim,
					"gt escalate -s medium",
					"No flag skips the gate.",
					"Do not run container suites yourself. Run the non-container packages, then",
				)
			}
			for _, want := range wants {
				if !strings.Contains(text, want) {
					t.Errorf("%s lacks %q", name, want)
				}
			}
			for _, gone := range []string{"--pre-verified", "--skip-tests", "--skip-verify"} {
				if strings.Contains(text, gone) {
					t.Errorf("%s still names %s, which gt done no longer has (ADR 0004)", name, gone)
				}
			}
		})
	}
}

// The failure rule must name the escalation path and must not offer a way
// around the gate: gt done has no bypass flag (ADR 0004).
func TestPolecatSlotLoopGuidanceDoesNotOfferPreVerified(t *testing.T) {
	raw, err := GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	i := strings.Index(text, polecatFormulaDoneWaitVerbatim)
	if i < 0 {
		t.Fatal("the gt done wait sentence is missing, so this check has nothing to inspect")
	}
	window := text[i : i+1200]
	if !strings.Contains(window, "gt escalate -s medium") {
		t.Error("the failure rule does not name the escalation path")
	}
	if strings.Contains(window, "gt done --") && !strings.Contains(window, "gt done --help") {
		t.Error("the failure rule advertises a gt done flag as a way out")
	}
}
