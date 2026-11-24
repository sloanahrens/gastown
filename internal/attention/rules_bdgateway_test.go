package attention

import "testing"

// TestBDGatewayCapHitRegistered is the cap-hit rule's place in the registry
// tables, before anything raises it: Kinds() lists the kind for readers and the
// rule checks, its items resolve to bd_gateway.cap_hit, the rule is a warn the
// overseer owns that wakes the operator like dispatch.idle, and the owner table
// names the overseer (gt-7oy397).
func TestBDGatewayCapHitRegistered(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindBDGatewayCapHit] {
		t.Fatalf("Kinds() does not list %q, so readers and the rule checks skip it", KindBDGatewayCapHit)
	}

	if got := RuleFor(Item{Kind: KindBDGatewayCapHit, Key: "bd-gateway-cap-hit"}); got != RuleBDGatewayCapHit {
		t.Errorf("RuleFor(%q) = %q, want %q", KindBDGatewayCapHit, got, RuleBDGatewayCapHit)
	}

	f, ok := Flags(RuleBDGatewayCapHit)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so the rule has no class", RuleBDGatewayCapHit)
	}
	if f != flagsWake {
		t.Errorf("Flags(%q) = %+v, want flagsWake %+v", RuleBDGatewayCapHit, f, flagsWake)
	}

	// RuleOwner falls back to the overseer for a rule with no row, so the row
	// itself is read: a missing row must fail here, not pass as the default.
	if owner, ok := ruleOwners[RuleBDGatewayCapHit]; !ok || owner != OwnerOverseer {
		t.Errorf("ruleOwners[%q] = %q (present %v), want %q", RuleBDGatewayCapHit, owner, ok, OwnerOverseer)
	}
}
