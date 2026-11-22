package attention

import "testing"

// main-unknown (gt-glc2yh): the rig's red-main check could not judge main for
// several runs in a row. The collector that raises it is a sibling bead; this
// file pins the kind's place in the three tables it needs before anything can
// raise or read it.

// TestMainUnknownKindMapped is the kind's place in the tables: Kinds() lists it
// for readers and the rules checks, its items resolve to main.unknown, and the
// rule carries exactly main.red's class, because a rig nobody can judge is as
// loud as one judged red (gt-glc2yh).
func TestMainUnknownKindMapped(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindMainUnknown] {
		t.Fatalf("Kinds() does not list %q, so readers and the rule checks skip it", KindMainUnknown)
	}

	for _, key := range []string{"main-unknown:gastown", "main-unknown:town"} {
		if got := RuleFor(Item{Kind: KindMainUnknown, Key: key}); got != RuleMainUnknown {
			t.Errorf("RuleFor(%q with key %q) = %q, want %q", KindMainUnknown, key, got, RuleMainUnknown)
		}
	}

	got, ok := Flags(RuleMainUnknown)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so the rule has no class", RuleMainUnknown)
	}
	want, ok := Flags(RuleRedMain)
	if !ok {
		t.Fatalf("Flags(%q) has no row", RuleRedMain)
	}
	if got != want {
		t.Errorf("Flags(%q) = %+v, want main.red's class %+v", RuleMainUnknown, got, want)
	}
	if !got.Wake || got.Handled != HandledOperatorAction {
		t.Errorf("Flags(%q) = %+v, want operator_action and wake", RuleMainUnknown, got)
	}
}

// TestMainUnknownRuleOwner is the rule's own owner row: RuleOwner answers the
// overseer for any unknown main.* rule, so the row is read from the table
// itself to stay red when it is deleted (gt-glc2yh).
func TestMainUnknownRuleOwner(t *testing.T) {
	t.Parallel()

	got, ok := ruleOwners[RuleMainUnknown]
	if !ok {
		t.Fatalf("the owner table has no row for %q", RuleMainUnknown)
	}
	if got != OwnerOverseer {
		t.Errorf("the owner table gives %q to %q, want %q", RuleMainUnknown, got, OwnerOverseer)
	}
	if owner := RuleOwner(RuleMainUnknown); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleMainUnknown, owner, OwnerOverseer)
	}
}
