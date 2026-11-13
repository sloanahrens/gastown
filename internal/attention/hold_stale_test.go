package attention

import "testing"

// TestHoldStaleItemRaisesTheWakingRule: a held bead nobody has released resolves
// to hold.stale, which wakes the overseer so the hold is not forgotten
// (gt-j669b4.19).
func TestHoldStaleItemRaisesTheWakingRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindHoldStale, Key: "hold-stale:gt-abc12"}
	rule := RuleFor(it)
	if rule != RuleHoldStale {
		t.Fatalf("RuleFor(hold-stale item) = %q, want %q", rule, RuleHoldStale)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action and wake", rule, f, ok)
	}
}

// TestHoldStaleItemIsTheOverseersToRelease: the overseer reads the hold's reason
// and releases it, so the rule's owner is the overseer, not the human.
func TestHoldStaleItemIsTheOverseersToRelease(t *testing.T) {
	t.Parallel()
	if owner := RuleOwner(RuleHoldStale); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", RuleHoldStale, owner, OwnerOverseer)
	}
}
