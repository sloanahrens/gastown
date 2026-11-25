package attention

import "testing"

// TestBdGatewayRefusalsRuleMapped pins the refusals rule's place in the kind,
// rule and flags tables: Kinds() lists the kind, its items resolve to
// bd_gateway.refusals, and the rule carries the cap-hit rule's flags, so a
// refused call wakes the operator the way a cap-hit does (gt-q0eh0r).
func TestBdGatewayRefusalsRuleMapped(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindBdGatewayRefusals] {
		t.Fatalf("Kinds() does not list %q, so readers and the rule checks skip it", KindBdGatewayRefusals)
	}

	if got := RuleFor(Item{Kind: KindBdGatewayRefusals, Key: "bd-gateway-refusals"}); got != RuleBdGatewayRefusals {
		t.Errorf("RuleFor(%q) = %q, want %q", KindBdGatewayRefusals, got, RuleBdGatewayRefusals)
	}

	f, ok := Flags(RuleBdGatewayRefusals)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so the rule has no class", RuleBdGatewayRefusals)
	}
	if f != flagsWake {
		t.Errorf("Flags(%q) = %+v, want flagsWake %+v", RuleBdGatewayRefusals, f, flagsWake)
	}
	capHit, ok := Flags(RuleBDGatewayCapHit)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so the parity check has no reference", RuleBDGatewayCapHit)
	}
	if f != capHit {
		t.Errorf("Flags(%q) = %+v, want the cap-hit rule's %+v", RuleBdGatewayRefusals, f, capHit)
	}
}

// TestBdGatewayRefusalsRuleOwner pins the refusals rule's owner: the overseer
// finds the refused caller before a person is woken. RuleOwner falls back to
// the overseer for a rule with no row, so the owner table row itself is read
// (gt-q0eh0r).
func TestBdGatewayRefusalsRuleOwner(t *testing.T) {
	t.Parallel()

	if owner, ok := ruleOwners[RuleBdGatewayRefusals]; !ok || owner != OwnerOverseer {
		t.Errorf("ruleOwners[%q] = %q (present %v), want %q", RuleBdGatewayRefusals, owner, ok, OwnerOverseer)
	}
	if owner := RuleOwner(RuleBdGatewayRefusals); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleBdGatewayRefusals, owner, OwnerOverseer)
	}
}
