package attention

import "testing"

// TestDeployPairStuckKindIsInTheKindsListAndOverseers is the stuck pair's place
// in the tables: a kind readers know, one rule of its own, and an explicit
// owner row giving it to the overseer. A kind missing from Kinds() is skipped
// by readers and the rule checks; a rule without an owner row is a queue row
// with no reader (gt-afwjb4).
func TestDeployPairStuckKindIsInTheKindsListAndOverseers(t *testing.T) {
	t.Parallel()

	found := false
	for _, k := range Kinds() {
		if k == KindDeployPairStuck {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Kinds() does not list %q, so readers and the rule check skip it", KindDeployPairStuck)
	}

	item := Item{Kind: KindDeployPairStuck, Key: "deploy-pair-stuck:gastown:0123456789ab"}
	rule := RuleFor(item)
	if rule != RuleDeployPairStuck {
		t.Fatalf("RuleFor(%q) = %q, want %q", item.Kind, rule, RuleDeployPairStuck)
	}

	owner, ok := ruleOwners[RuleDeployPairStuck]
	if !ok {
		t.Fatalf("the owner table has no row for %q", RuleDeployPairStuck)
	}
	if owner != OwnerOverseer {
		t.Errorf("the owner table gives %q to %q, want %q", RuleDeployPairStuck, owner, OwnerOverseer)
	}
	if got := RuleOwner(rule); got != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", rule, got, OwnerOverseer)
	}
}

// TestDeployPairStuckRaisesTheWakingRule: a green commit whose pair is still
// unpublished past the stuck bound resolves to deploy.pair_stuck, which wakes
// the overseer for an operator action. A row that queued would leave a commit
// nothing rolls onto by itself, with no images run to retry, until someone
// happened to look (gt-afwjb4).
func TestDeployPairStuckRaisesTheWakingRule(t *testing.T) {
	t.Parallel()

	f, ok := Flags(RuleDeployPairStuck)
	if !ok {
		t.Fatalf("Flags(%s) has no row in the flags table", RuleDeployPairStuck)
	}
	if !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v; want operator_action and wake", RuleDeployPairStuck, f)
	}
}
