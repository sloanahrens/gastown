package attention

import "testing"

// TestRedHoldRuleKindMapsToTheWakingRule is the red hold kind's place in the
// tables: Kinds() lists it, its items resolve to hold.red, and the rule wakes
// the overseer, because a hold nobody lifts stops dispatch (gt-7j8krh).
func TestRedHoldRuleKindMapsToTheWakingRule(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindRedHoldHeld] {
		t.Fatalf("Kinds() does not list %q, so readers and the rule checks skip it", KindRedHoldHeld)
	}
	if got := RuleFor(Item{Kind: KindRedHoldHeld, Key: "hold:red"}); got != RuleRedHold {
		t.Errorf("RuleFor(red-hold item) = %q, want %q", got, RuleRedHold)
	}
	f, ok := Flags(RuleRedHold)
	if !ok {
		t.Fatalf("Flags(%q) has no row in the flags table", RuleRedHold)
	}
	if !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%q) = %+v, want operator_action and wake", RuleRedHold, f)
	}
}

// TestRedHoldRuleOwnerIsTheOverseer reads the owner row from the table itself,
// so deleting it turns this red instead of falling back (gt-7j8krh).
func TestRedHoldRuleOwnerIsTheOverseer(t *testing.T) {
	t.Parallel()

	got, ok := ruleOwners[RuleRedHold]
	if !ok {
		t.Fatalf("the owner table has no row for %q", RuleRedHold)
	}
	if got != OwnerOverseer {
		t.Errorf("the owner table gives %q to %q, want %q", RuleRedHold, got, OwnerOverseer)
	}
	if owner := RuleOwner(RuleRedHold); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleRedHold, owner, OwnerOverseer)
	}
}
