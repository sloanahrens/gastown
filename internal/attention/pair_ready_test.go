package attention

import "testing"

// TestPairReadyItemRaisesTheWakingRule: a pair-ready item resolves to the
// deploy.pair_ready rule, which wakes the overseer for an operator action and
// belongs to the overseer. A rule that fell to info would leave a roll that can
// start waiting until someone looked (gt-4nlpw8).
func TestPairReadyItemRaisesTheWakingRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindDeployPairReady, Key: "deploy-pair-ready:gastown:0123456789ab"}
	rule := RuleFor(it)
	if rule != RuleDeployPairReady {
		t.Fatalf("RuleFor(pair-ready item) = %q, want %q", rule, RuleDeployPairReady)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action and wake", rule, f, ok)
	}
	if owner := RuleOwner(rule); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", rule, owner, OwnerOverseer)
	}
}
