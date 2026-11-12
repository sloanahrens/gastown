package attention

import "testing"

// TestConfigEnvShadowedItemRaisesTheAskRule: a settings daemon.env key the
// container shadows resolves to config.env_shadowed, which asks the operator
// action without waking anyone, and belongs to the overseer, who removes the
// stale settings entry. A rule that fell to info would leave the stale entry
// standing; a rule nobody owns is a queue row with no reader (gt-ih19g6).
func TestConfigEnvShadowedItemRaisesTheAskRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindConfigEnvShadowed, Key: configEnvShadowedKeyForTest}
	rule := RuleFor(it)
	if rule != RuleConfigEnvShadowed {
		t.Fatalf("RuleFor(config-env-shadowed item) = %q, want %q", rule, RuleConfigEnvShadowed)
	}
	f, ok := Flags(rule)
	if !ok || f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action and no wake", rule, f, ok)
	}
	if owner := RuleOwner(rule); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", rule, owner, OwnerOverseer)
	}
}

// configEnvShadowedKeyForTest is the item key the daemon collector writes; the
// rule mapping reads only the kind, so the key is any non-empty value here.
const configEnvShadowedKeyForTest = "config-env-shadowed"
