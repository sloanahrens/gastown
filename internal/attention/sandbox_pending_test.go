package attention

import "testing"

// TestSandboxPendingItemRaisesTheWakingRule: a sandbox pod the scheduler cannot
// place resolves to sandbox.pending, which wakes the overseer. A rule that fell
// to ask would leave the seat's agent unstarted with no wake (gt-1gtq5g).
func TestSandboxPendingItemRaisesTheWakingRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindSandboxPending, Key: "sandbox-pending:default/default--gt-nux"}
	rule := RuleFor(it)
	if rule != RuleSandboxPending {
		t.Fatalf("RuleFor(sandbox-pending item) = %q, want %q", rule, RuleSandboxPending)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action and wake", rule, f, ok)
	}
}

// TestSandboxPendingItemIsTheOverseersToAct: the overseer gives the pod the
// resources its request needs, so the rule's owner is the overseer, not the
// human.
func TestSandboxPendingItemIsTheOverseersToAct(t *testing.T) {
	t.Parallel()
	if owner := RuleOwner(RuleSandboxPending); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", RuleSandboxPending, owner, OwnerOverseer)
	}
}

// TestSandboxPendingIsAKind: the kind is in the full set a reader must know, so
// a queue row of it is not an unknown kind (gt-1gtq5g).
func TestSandboxPendingIsAKind(t *testing.T) {
	t.Parallel()
	found := false
	for _, k := range Kinds() {
		if k == KindSandboxPending {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Kinds() does not name %q", KindSandboxPending)
	}
}
