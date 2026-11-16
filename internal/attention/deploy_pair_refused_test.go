package attention

import "testing"

// TestPreRollGuardDeployPairRefusedIsTheOverseersWakingRule: a refused pair
// resolves to deploy.pair_refused, which wakes the overseer for an operator
// action and belongs to it. A rule that fell to info would leave a commit the
// pre-swap check refused waiting until someone looked, and a kind missing from
// Kinds() is skipped by the rule and owner checks (gt-d6ok4r).
func TestPreRollGuardDeployPairRefusedIsTheOverseersWakingRule(t *testing.T) {
	t.Parallel()

	listed := false
	for _, k := range Kinds() {
		if k == KindDeployPairRefused {
			listed = true
		}
	}
	if !listed {
		t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", KindDeployPairRefused)
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
		{"unreadable", KindDeployPairUnreadable, RuleDeployPairUnreadable, false, HandledOperatorAction, OwnerOverseer},
		{"refused", KindDeployPairRefused, RuleDeployPairRefused, true, HandledOperatorAction, OwnerOverseer},
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
}
