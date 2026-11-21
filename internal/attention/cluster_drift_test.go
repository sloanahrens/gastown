package attention

import "testing"

// TestClusterDriftRules is the drift compare's place in the tables, one row per
// kind: each kind is listed for readers, resolves to its own rule, the rule is
// a warn that wakes no one, and the overseer reads what the compare found
// (gt-y705n1). A kind missing from Kinds() is skipped by the rules and owner
// checks, and a rule with no flags row has no class.
func TestClusterDriftRules(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}

	for _, tc := range []struct {
		name     string
		kind     Kind
		wantRule string
	}{
		{"drift", KindClusterDrift, RuleClusterDrift},
		{"unreadable", KindClusterDriftUnreadable, RuleClusterDriftUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !listed[tc.kind] {
				t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", tc.kind)
			}
			item := Item{Kind: tc.kind, Key: "cluster-drift:gastown"}
			if got := RuleFor(item); got != tc.wantRule {
				t.Fatalf("RuleFor(%q) = %q, want %q", tc.kind, got, tc.wantRule)
			}
			f, ok := Flags(tc.wantRule)
			if !ok {
				t.Fatalf("Flags(%q) has no row", tc.wantRule)
			}
			if f.Wake || f.Handled != HandledOperatorAction {
				t.Errorf("Flags(%q) = %+v, want operator_action and no wake", tc.wantRule, f)
			}
			if owner := RuleOwner(tc.wantRule); owner != OwnerOverseer {
				t.Errorf("RuleOwner(%q) = %q, want %q", tc.wantRule, owner, OwnerOverseer)
			}
		})
	}
}
