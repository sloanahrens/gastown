package attention

import "testing"

// TestBDGatewayUnreachableRegistered pins the judge's kind, rule, flags and
// owner (gt-g7rdh4).
func TestBDGatewayUnreachableRegistered(t *testing.T) {
	t.Parallel()

	listed := false
	for _, k := range Kinds() {
		listed = listed || k == KindBDGatewayUnreachable
	}
	if !listed {
		t.Errorf("Kinds() does not list %q", KindBDGatewayUnreachable)
	}
	if got := RuleFor(Item{Kind: KindBDGatewayUnreachable}); got != RuleBDGatewayUnreachable {
		t.Errorf("RuleFor(%q) = %q, want %q", KindBDGatewayUnreachable, got, RuleBDGatewayUnreachable)
	}
	if f, ok := Flags(RuleBDGatewayUnreachable); !ok || f != flagsWake {
		t.Errorf("Flags(%q) = %+v (found %v), want flagsWake", RuleBDGatewayUnreachable, f, ok)
	}
	// RuleOwner falls back to the overseer, so the row itself is read.
	if owner, ok := ruleOwners[RuleBDGatewayUnreachable]; !ok || owner != OwnerOverseer {
		t.Errorf("ruleOwners[%q] = %q (present %v), want %q", RuleBDGatewayUnreachable, owner, ok, OwnerOverseer)
	}
}

// TestGatewayUnreachableItems is the growth arithmetic and the readings that
// must raise nothing (gt-g7rdh4).
func TestGatewayUnreachableItems(t *testing.T) {
	t.Parallel()

	read := func(v uint64) *uint64 { return &v }
	for _, tc := range []struct {
		name    string
		prev    *uint64
		cur     *uint64
		summary string
	}{
		{"a burst of ten", read(5), read(15), "bd gateway: 10 calls could not reach its database since the previous read (exit 25)"},
		{"a burst from zero", read(0), read(12), "bd gateway: 12 calls could not reach its database since the previous read (exit 25)"},
		{"nine is below the bound", read(5), read(14), ""},
		{"single failure", read(5), read(6), ""},
		{"the counter fell", read(50), read(3), ""},
		{"first reading", nil, read(75), ""},
		{"gateway from before the counter", read(5), nil, ""},
		{"neither reading", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			items := GatewayUnreachableItems(tc.prev, tc.cur)
			if tc.summary == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Kind != KindBDGatewayUnreachable || it.Severity != SeverityMedium || it.Key != RuleBDGatewayUnreachable {
				t.Errorf("item = %+v, want a medium %q item keyed %q", it, KindBDGatewayUnreachable, RuleBDGatewayUnreachable)
			}
			if it.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", it.Summary, tc.summary)
			}
		})
	}
}
