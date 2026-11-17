package attention

import "testing"

// TestPreRollGuardDeployPairSkewIsTheOverseersWakingRule: a deploy pair whose bd
// pin carries a schema level the database does not raises deploy.pair_skew,
// which wakes the overseer for an operator action and belongs to it. A rule
// that fell to info would leave the skewed pair waiting until someone looked,
// and a kind missing from Kinds() is skipped by the rule and owner checks
// (gt-qnjuqp).
func TestPreRollGuardDeployPairSkewIsTheOverseersWakingRule(t *testing.T) {
	t.Parallel()

	listed := false
	for _, k := range Kinds() {
		if k == KindDeployPairSkew {
			listed = true
		}
	}
	if !listed {
		t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", KindDeployPairSkew)
	}

	it := Item{Kind: KindDeployPairSkew, Key: "deploy-pair-skew:gastown:0123456789ab"}
	rule := RuleFor(it)
	if rule != RuleDeployPairSkew {
		t.Fatalf("RuleFor(%q) = %q, want %q", it.Kind, rule, RuleDeployPairSkew)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%q) = %+v, %v; want wake and %q", rule, f, ok, HandledOperatorAction)
	}
	if owner := RuleOwner(rule); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", rule, owner, OwnerOverseer)
	}
}
