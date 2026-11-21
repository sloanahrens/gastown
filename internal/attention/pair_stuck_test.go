package attention

import "testing"

// TestDeployPairStuckKindIsInTheKindsListAndOverseers is the deploy pair
// family's place in the kind, rule and owner tables, one row per kind: each is
// listed for readers, resolves to a rule of its own, and has an owner row
// giving it to the overseer. A kind missing from Kinds() is skipped by readers
// and the rule checks; a rule without an owner row is a queue row with no
// reader (gt-afwjb4).
func TestDeployPairStuckKindIsInTheKindsListAndOverseers(t *testing.T) {
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
		{"ready", KindDeployPairReady, RuleDeployPairReady},
		{"unreadable", KindDeployPairUnreadable, RuleDeployPairUnreadable},
		{"stuck", KindDeployPairStuck, RuleDeployPairStuck},
		{"refused", KindDeployPairRefused, RuleDeployPairRefused},
		{"skew", KindDeployPairSkew, RuleDeployPairSkew},
		{"golist degraded", KindDeployPairGoListDegraded, RuleDeployPairGoListDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !listed[tc.kind] {
				t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", tc.kind)
			}

			item := Item{Kind: tc.kind, Key: string(tc.kind) + ":gastown:0123456789ab"}
			if got := RuleFor(item); got != tc.wantRule {
				t.Fatalf("RuleFor(%q) = %q, want %q", tc.kind, got, tc.wantRule)
			}

			// RuleOwner falls back to the overseer for a rule with no row, so
			// the row itself is what this reads (gt-afwjb4).
			owner, ok := ruleOwners[tc.wantRule]
			if !ok {
				t.Fatalf("the owner table has no row for %q", tc.wantRule)
			}
			if owner != OwnerOverseer {
				t.Errorf("the owner table gives %q to %q, want %q", tc.wantRule, owner, OwnerOverseer)
			}
			if got := RuleOwner(tc.wantRule); got != OwnerOverseer {
				t.Errorf("RuleOwner(%s) = %q, want %q", tc.wantRule, got, OwnerOverseer)
			}
		})
	}
}

// TestDeployPairStuckRaisesTheWakingRule is the deploy pair family's severity
// class, one row per kind: the stuck pair wakes the overseer for an operator
// action like the ready, refused and skewed pairs, while the unreadable read
// queues without waking. A stuck row that queued would leave a green commit
// nothing rolls onto by itself, with no images run to retry, until someone
// happened to look (gt-afwjb4).
func TestDeployPairStuckRaisesTheWakingRule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		rule        string
		wantWake    bool
		wantHandled Handled
	}{
		{"ready", RuleDeployPairReady, true, HandledOperatorAction},
		{"unreadable", RuleDeployPairUnreadable, false, HandledOperatorAction},
		{"stuck", RuleDeployPairStuck, true, HandledOperatorAction},
		{"refused", RuleDeployPairRefused, true, HandledOperatorAction},
		{"skew", RuleDeployPairSkew, true, HandledOperatorAction},
		{"golist degraded", RuleDeployPairGoListDegraded, false, HandledInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := Flags(tc.rule)
			if !ok {
				t.Fatalf("Flags(%q) has no row in the flags table", tc.rule)
			}
			if f.Wake != tc.wantWake || f.Handled != tc.wantHandled {
				t.Errorf("Flags(%q) = %+v, want wake %v and %q", tc.rule, f, tc.wantWake, tc.wantHandled)
			}
		})
	}
}
