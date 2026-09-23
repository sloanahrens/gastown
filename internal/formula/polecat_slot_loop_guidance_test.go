package formula

import (
	"strings"
	"testing"
)

// polecatFormulaSlotLoopVerbatim is the instruction gt-7dxw requires the
// polecat workflow to carry: `gt done` waits for the container-gate slot on
// its own, a polecat never polls the slot or scripts a retry around it, and a
// gate failure goes to the mayor as a one-shot --skip-verify ruling rather
// than into an improvised loop.
//
// It is asserted character-for-character because a polecat misreads the wait
// as a hang and bd-closes its bead mid-`gt done` (overseer hq-wisp-6q5ib).
// The calm-wait sentence replaced an earlier "may sit silently for up to 20-30
// minutes": that claim came from the same ruling that asked for the 2-minute
// progress line, so the pane is not silent and the wait is bounded by the cap
// (gt-7dxw review).
const polecatFormulaSlotLoopVerbatim = "gt done waits for the container-gate slot before it runs the " +
	"container suites, printing a `still waiting for the container-gate slot …` line every couple of " +
	"minutes while it does. That is normal. Do not interrupt it, do not close the bead, do not retry. " +
	"It gives up with a slot-acquire timeout once the cap expires."

// TestPolecatFormulasCarryTheSlotLoopRule guards the three polecat workflows
// that run `gt done`: the default, the monorepo/fork variant, and the doc
// audit (a hand-maintained copy of the default's step text). A polecat under
// any of them hits the same slot and the same temptation, so all three must
// carry the rule — the copy that drifts is the one an agent reads.
//
// Only the default carries it in full. The two variants carry the rule's
// trigger and a pointer to the one home, because three hand-maintained copies
// of the same paragraph is the drift this test would otherwise be pinning
// (gt-7dxw review, R2).
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
				"Never poll the slot",
				"docs/reference.md",
			}
			if name == "mol-polecat-work" {
				wants = append(wants,
					polecatFormulaSlotLoopVerbatim,
					"gt escalate -s medium",
					"--skip-verify",
					"Do not run container suites yourself. Run the non-container packages, then",
				)
			}
			for _, want := range wants {
				if !strings.Contains(text, want) {
					t.Errorf("%s lacks %q", name, want)
				}
			}
		})
	}
}

// The guidance must not name an unsanctioned escape hatch: `--pre-verified` is
// the refinery's and mayor's flag, and a polecat reaching for it instead of
// escalating re-runs the same gate set under the same slot cap (the formula's
// own speed principle, gt-pnkd).
func TestPolecatSlotLoopGuidanceDoesNotOfferPreVerified(t *testing.T) {
	raw, err := GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	i := strings.Index(text, polecatFormulaSlotLoopVerbatim)
	if i < 0 {
		t.Fatal("the slot-wait sentence is missing, so this check has nothing to inspect")
	}
	// The paragraph following the sentence is the retry rule; it must name the
	// escalation path and must not offer --pre-verified as the way out.
	window := text[i : i+1800]
	if !strings.Contains(window, "gt escalate -s medium") {
		t.Error("the slot-loop rule does not name the escalation path")
	}
	if strings.Contains(window, "gt done --pre-verified") {
		t.Error("the slot-loop rule advertises --pre-verified, which is refinery/mayor only")
	}
}
