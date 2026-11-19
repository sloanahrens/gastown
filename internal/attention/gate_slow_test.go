package attention

import "testing"

// TestGateSlowItemRaisesTheWakingRule: a gate-slow item resolves to gate.slow,
// and that rule has a flags row that wakes the overseer.
func TestGateSlowItemRaisesTheWakingRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindGateSlow, Key: "gate-slow:gastown"}
	rule := RuleFor(it)
	if rule != RuleGateSlow {
		t.Fatalf("RuleFor(gate-slow item) = %q, want %q", rule, RuleGateSlow)
	}
	f, ok := Flags(rule)
	if !ok {
		t.Fatalf("rule %q has no flags row", rule)
	}
	if !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, want operator_action and wake", rule, f)
	}
}

// TestGateSlowItemIsTheOverseersToAct: the overseer reads the gate runs and
// decides what slowed the gate before a person is woken.
func TestGateSlowItemIsTheOverseersToAct(t *testing.T) {
	t.Parallel()
	if owner := RuleOwner(RuleGateSlow); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", RuleGateSlow, owner, OwnerOverseer)
	}
}
