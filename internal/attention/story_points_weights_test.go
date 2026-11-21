package attention

import "testing"

// TestStoryPointsRuleMapped is the weights kind's place in the tables: Kinds()
// lists it for readers and the rule checks, its items resolve to
// story_points.weights whatever the adoption collector keys them by, and the
// rule is an info record that wakes no one (gt-hlggvh).
func TestStoryPointsRuleMapped(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindStoryPoints] {
		t.Fatalf("Kinds() does not list %q, so readers and the rule checks skip it", KindStoryPoints)
	}

	for _, key := range []string{"story-points:adopted", "story-points:reverted", "story-points:refused"} {
		if got := RuleFor(Item{Kind: KindStoryPoints, Key: key}); got != RuleStoryPoints {
			t.Errorf("RuleFor(%q with key %q) = %q, want %q", KindStoryPoints, key, got, RuleStoryPoints)
		}
	}

	f, ok := Flags(RuleStoryPoints)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so the rule has no class", RuleStoryPoints)
	}
	if f.Wake || f.Handled != HandledInfo {
		t.Errorf("Flags(%q) = %+v, want info and no wake", RuleStoryPoints, f)
	}
}

// TestStoryPointsRuleOwner is the weights rule's own owner row: RuleOwner
// answers the overseer for any unknown story_points.* rule, so the row is read
// from the table itself to stay red when it is deleted (gt-hlggvh).
func TestStoryPointsRuleOwner(t *testing.T) {
	t.Parallel()

	got, ok := ruleOwners[RuleStoryPoints]
	if !ok {
		t.Fatalf("the owner table has no row for %q", RuleStoryPoints)
	}
	if got != OwnerOverseer {
		t.Errorf("the owner table gives %q to %q, want %q", RuleStoryPoints, got, OwnerOverseer)
	}
	if owner := RuleOwner(RuleStoryPoints); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleStoryPoints, owner, OwnerOverseer)
	}
}
