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

	for _, tc := range []struct {
		name        string
		kind        Kind
		wantRule    string
		wantWake    bool
		wantHandled Handled
		wantOwner   Owner
	}{
		{"ready", KindDeployPairReady, RuleDeployPairReady, true, HandledOperatorAction, OwnerOverseer},
		{"stuck", KindDeployPairStuck, RuleDeployPairStuck, true, HandledOperatorAction, OwnerOverseer},
		{"refused", KindDeployPairRefused, RuleDeployPairRefused, true, HandledOperatorAction, OwnerOverseer},
		{"skew", KindDeployPairSkew, RuleDeployPairSkew, true, HandledOperatorAction, OwnerOverseer},
	} {
		it := Item{Kind: tc.kind, Key: string(tc.kind) + ":gastown:0123456789ab"}
		rule := RuleFor(it)
		if rule != tc.wantRule {
			t.Errorf("%s: RuleFor(%q) = %q, want %q", tc.name, tc.kind, rule, tc.wantRule)
			continue
		}
		f, ok := Flags(rule)
		if !ok || f.Wake != tc.wantWake || f.Handled != tc.wantHandled {
			t.Errorf("%s: Flags(%q) = %+v, %v; want wake %v and %q", tc.name, rule, f, ok, tc.wantWake, tc.wantHandled)
		}
		if owner := RuleOwner(rule); owner != tc.wantOwner {
			t.Errorf("%s: RuleOwner(%q) = %q, want %q", tc.name, rule, owner, tc.wantOwner)
		}
	}

	// RuleOwner falls back to the overseer for a rule with no row, so the row
	// itself is what this reads: the owner table has one row per rule, and a
	// rule missing from it is a row nobody owns (gt-qnjuqp).
	if got, ok := ruleOwners[RuleDeployPairSkew]; !ok || got != OwnerOverseer {
		t.Errorf("the owner table has no overseer row for %q: got %q, present %v", RuleDeployPairSkew, got, ok)
	}
}
