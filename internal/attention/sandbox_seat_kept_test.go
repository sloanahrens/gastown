package attention

import "testing"

// TestSandboxSeatKeptItemRaisesTheWakingRule: a kept sandbox seat past its
// window resolves to sandbox.seat_kept, which wakes the overseer. A rule that
// fell to ask would leave a seat holding its slot with no wake (gt-l38v7r).
func TestSandboxSeatKeptItemRaisesTheWakingRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindSandboxSeatKept, Key: "sandbox-seat-kept:gastown/furiosa"}
	rule := RuleFor(it)
	if rule != RuleSandboxSeatKept {
		t.Fatalf("RuleFor(sandbox-seat-kept item) = %q, want %q", rule, RuleSandboxSeatKept)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action and wake", rule, f, ok)
	}
}

// TestSandboxSeatKeptItemIsTheOverseersToAct: the overseer reads the keep reason
// and clears it, so the rule's owner is the overseer, not the human.
func TestSandboxSeatKeptItemIsTheOverseersToAct(t *testing.T) {
	t.Parallel()
	if owner := RuleOwner(RuleSandboxSeatKept); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", RuleSandboxSeatKept, owner, OwnerOverseer)
	}
}
