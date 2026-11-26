package attention

import "testing"

// TestGateThrottleUnreadItemIsLoudAndTheOverseers: a gate-throttle-unread item
// resolves to gate.throttle, which wakes the overseer and is the overseer's to
// act on, so a rig whose throttle read failed for every recent run is not only
// a per-run log line (gt-rqodz6).
func TestGateThrottleUnreadItemIsLoudAndTheOverseers(t *testing.T) {
	t.Parallel()
	listed := false
	for _, k := range Kinds() {
		if k == KindGateThrottleUnread {
			listed = true
		}
	}
	if !listed {
		t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", KindGateThrottleUnread)
	}

	rule := RuleFor(Item{Kind: KindGateThrottleUnread, Key: "gate-throttle-unread:gastown"})
	if rule != RuleGateThrottle {
		t.Fatalf("RuleFor(gate-throttle-unread item) = %q, want %q", rule, RuleGateThrottle)
	}
	if f, ok := Flags(rule); !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %t, want operator_action and wake", rule, f, ok)
	}
	if owner := RuleOwner(RuleGateThrottle); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", RuleGateThrottle, owner, OwnerOverseer)
	}
}
