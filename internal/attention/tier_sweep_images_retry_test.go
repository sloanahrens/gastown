package attention

import "testing"

// TestTierSweepImagesRetryExhaustedKind is the exhausted-retries item's place
// in the tables: a kind listed for readers, with a rule of its own. A kind
// missing from Kinds() is skipped by the rules and owners checks and gets no
// metrics column, and a kind with no rule id resolves to no rule at all
// (gt-j6cwqr).
func TestTierSweepImagesRetryExhaustedKind(t *testing.T) {
	t.Parallel()

	found := false
	for _, k := range Kinds() {
		if k == KindTierSweepImagesRetryExhausted {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", KindTierSweepImagesRetryExhausted)
	}

	item := Item{Kind: KindTierSweepImagesRetryExhausted, Key: "tier-sweep-images-retry-exhausted:gastown:0123456789ab"}
	if got := RuleFor(item); got != RuleTierSweepImagesRetryExhausted {
		t.Errorf("RuleFor(%q) = %q, want %q", item.Kind, got, RuleTierSweepImagesRetryExhausted)
	}
}

// TestTierSweepImagesRetryExhaustedWakesLikeImagesFailed pins the exhausted
// item to the class of the first failure it follows: the same rule flags, and
// those flags are an operator_action that wakes. Every early re-run is spent
// by the time it is raised, so nothing retries the build until the next hourly
// cycle; a row that queued it quietly would leave that commit waiting
// (gt-j6cwqr).
func TestTierSweepImagesRetryExhaustedWakesLikeImagesFailed(t *testing.T) {
	t.Parallel()

	want, ok := Flags(RuleImagesFailed)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so there is no class to match", RuleImagesFailed)
	}
	got, ok := Flags(RuleTierSweepImagesRetryExhausted)
	if !ok {
		t.Fatalf("Flags(%q) has no row", RuleTierSweepImagesRetryExhausted)
	}
	if got != want {
		t.Errorf("Flags(%q) = %+v, want the first failure's class %+v", RuleTierSweepImagesRetryExhausted, got, want)
	}
	if !got.Wake || got.Handled != HandledOperatorAction {
		t.Errorf("Flags(%q) = %+v, want operator_action and wake", RuleTierSweepImagesRetryExhausted, got)
	}
}

// TestTierSweepImagesRetryExhaustedIsTheOverseers pins the row to the overseer:
// the images workflow is the town's own to re-run or fix before a person is
// woken, the same as its first failure. The row is read from the table
// directly because RuleOwner answers the same value by family fallback, which
// would hide a deleted row from every test that reads only the answer
// (gt-j6cwqr).
func TestTierSweepImagesRetryExhaustedIsTheOverseers(t *testing.T) {
	t.Parallel()

	got, ok := ruleOwners[RuleTierSweepImagesRetryExhausted]
	if !ok {
		t.Fatalf("the owner table has no row for %q", RuleTierSweepImagesRetryExhausted)
	}
	if got != OwnerOverseer {
		t.Errorf("the owner table gives %q to %q, want %q", RuleTierSweepImagesRetryExhausted, got, OwnerOverseer)
	}
	if owner := RuleOwner(RuleTierSweepImagesRetryExhausted); owner != got {
		t.Errorf("RuleOwner(%q) = %q, want the table's %q", RuleTierSweepImagesRetryExhausted, owner, got)
	}
}
