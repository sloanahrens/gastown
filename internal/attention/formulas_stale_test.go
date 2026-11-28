package attention

import "testing"

// TestFormulasStaleKindResolvesToItsRule: a formulas-stale item resolves to
// formulas.stale. A kind left out of the rule table resolves to "", and a
// queue row with no rule carries no class for any reader (gt-glqv7r).
func TestFormulasStaleKindResolvesToItsRule(t *testing.T) {
	t.Parallel()

	it := Item{Kind: KindFormulasStale, Key: formulasStaleKeyForTest}
	if got := RuleFor(it); got != RuleFormulasStale {
		t.Errorf("RuleFor(%q) = %q, want %q", KindFormulasStale, got, RuleFormulasStale)
	}
}

// TestFormulasStaleRuleWakesTheOverseer: a stale town formula wakes, because
// no run rewrites the file itself, and the overseer owns it, because
// reconciling the town's copy with the binary's is the town's own work. The
// owner row is read directly rather than through RuleOwner, whose fallback is
// the overseer and would hide a missing row (gt-glqv7r).
func TestFormulasStaleRuleWakesTheOverseer(t *testing.T) {
	t.Parallel()

	f, ok := Flags(RuleFormulasStale)
	if !ok {
		t.Fatalf("Flags(%q) has no row", RuleFormulasStale)
	}
	if !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%q) = %+v, want operator_action and wake", RuleFormulasStale, f)
	}
	if owner, ok := ruleOwners[RuleFormulasStale]; !ok || owner != OwnerOverseer {
		t.Errorf("ruleOwners[%q] = %q (present %v), want %q", RuleFormulasStale, owner, ok, OwnerOverseer)
	}
}

// TestFormulasStaleKindIsListed: Kinds() lists formulas-stale. A kind missing
// from Kinds() is skipped by the rule and owner checks and by every reader
// that walks the full set (gt-glqv7r).
func TestFormulasStaleKindIsListed(t *testing.T) {
	t.Parallel()

	for _, k := range Kinds() {
		if k == KindFormulasStale {
			return
		}
	}
	t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", KindFormulasStale)
}

// formulasStaleKeyForTest is any non-empty item key: the rule mapping reads
// only the kind.
const formulasStaleKeyForTest = "formulas-stale:mol-polecat-work"
