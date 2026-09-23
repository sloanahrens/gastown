package formula

import (
	"strings"
	"testing"
)

// polecatFormulaSlotLoopGuidance is the instruction gt-7dxw requires the
// polecat workflow to carry: `gt done` waits for the container-gate slot on
// its own, a polecat never polls the slot or scripts a retry around it, and a
// gate failure goes to the mayor as a one-shot --skip-verify ruling rather
// than into an improvised loop.
//
// The verbatim sentence is the one the ruling fixed (overseer hq-wisp-6q5ib: a
// third polecat misread the slot wait as a hang and bd-closed its bead
// mid-`gt done`), so it is asserted character-for-character rather than
// loosely.
const polecatFormulaSlotLoopVerbatim = "gt done may sit silently for up to 20-30 minutes waiting for the " +
	"container-gate slot. That is normal. Do not interrupt it, do not close the bead, do not retry. " +
	"It will print a slot-acquire timeout if it gives up."

// TestPolecatFormulasCarryTheSlotLoopRule guards the three polecat workflows
// that run `gt done`: the default, the monorepo/fork variant, and the doc
// audit (a hand-maintained copy of the default's step text). A polecat under
// any of them hits the same slot and the same temptation, so all three must
// carry the rule — the copy that drifts is the one an agent reads.
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
			if !strings.Contains(text, polecatFormulaSlotLoopVerbatim) {
				t.Errorf("%s lacks the verbatim slot-wait sentence", name)
			}
			for _, want := range []string{
				"Never poll the slot, and never script a retry around gt done",
				"gt escalate -s medium",
				"--skip-verify",
				"Do not run container suites yourself. Run the non-container packages, then",
			} {
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
