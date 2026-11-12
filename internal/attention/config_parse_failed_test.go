package attention

import "testing"

// TestConfigParseFailedItemWakesTheOverseer: a patrol config file that does not
// load on a heartbeat reload resolves to config.parse_failed. The daemon runs on
// the config it had, so the rule wakes the overseer, who fixes the file, and
// belongs to the overseer (gt-6frjqf).
func TestConfigParseFailedItemWakesTheOverseer(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindConfigParseFailed, Key: "config-parse-failed"}
	rule := RuleFor(it)
	if rule != RuleConfigParseFailed {
		t.Fatalf("RuleFor(config-parse-failed item) = %q, want %q", rule, RuleConfigParseFailed)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action with a wake", rule, f, ok)
	}
	if owner := RuleOwner(rule); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", rule, owner, OwnerOverseer)
	}
}
