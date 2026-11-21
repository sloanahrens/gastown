package attention

import "testing"

// TestOverrunBlindRules is the blind overrun guard's place in the tables, one
// row per kind: the kind is listed for readers, its item resolves to
// overrun.blind, the rule is a record that wakes no one, and the overseer reads
// what the guard could not (gt-uor98u). A kind missing from Kinds() is skipped
// by the rules and owner checks, and a rule with no flags row has no class.
func TestOverrunBlindRules(t *testing.T) {
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
		{"blind read", KindOverrunBlind, RuleOverrunBlind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !listed[tc.kind] {
				t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", tc.kind)
			}
			item := Item{Kind: tc.kind, Key: "overrun-blind:gastown"}
			if got := RuleFor(item); got != tc.wantRule {
				t.Fatalf("RuleFor(%q) = %q, want %q", tc.kind, got, tc.wantRule)
			}
			f, ok := Flags(tc.wantRule)
			if !ok {
				t.Fatalf("Flags(%q) has no row", tc.wantRule)
			}
			if f.Wake || f.Handled != HandledInfo {
				t.Errorf("Flags(%q) = %+v, want info and no wake", tc.wantRule, f)
			}
			if owner := RuleOwner(tc.wantRule); owner != OwnerOverseer {
				t.Errorf("RuleOwner(%q) = %q, want %q", tc.wantRule, owner, OwnerOverseer)
			}
		})
	}
}
