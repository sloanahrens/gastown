package attention

import "testing"

// TestTierSweepImagesRetryExhaustedKind is the images-retry family's place in
// the tables, one row per kind: each is listed for readers and resolves to a
// rule of its own. A kind missing from Kinds() is skipped by the rules and
// owners checks and gets no metrics column, and a kind with no rule id
// resolves to no rule at all (gt-j6cwqr).
func TestTierSweepImagesRetryExhaustedKind(t *testing.T) {
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
		{"images failed", KindImagesFailed, RuleImagesFailed},
		{"retries exhausted", KindTierSweepImagesRetryExhausted, RuleTierSweepImagesRetryExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !listed[tc.kind] {
				t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", tc.kind)
			}
			item := Item{Kind: tc.kind, Key: string(tc.kind) + ":gastown:0123456789ab"}
			if got := RuleFor(item); got != tc.wantRule {
				t.Errorf("RuleFor(%q) = %q, want %q", tc.kind, got, tc.wantRule)
			}
		})
	}
}

// TestTierSweepImagesRetryExhaustedWakesLikeImagesFailed pins the images-retry
// rules to the class of the first failure the exhausted item follows, one row
// per rule: the same flags, and those flags are an operator_action that wakes.
// Every early re-run is spent by the time the exhausted item is raised, so
// nothing retries the build until the next hourly cycle; a row that queued it
// quietly would leave that commit waiting (gt-j6cwqr).
func TestTierSweepImagesRetryExhaustedWakesLikeImagesFailed(t *testing.T) {
	t.Parallel()

	want, ok := Flags(RuleImagesFailed)
	if !ok {
		t.Fatalf("Flags(%q) has no row, so there is no class to match", RuleImagesFailed)
	}

	for _, tc := range []struct {
		name string
		rule string
	}{
		{"images failed", RuleImagesFailed},
		{"retries exhausted", RuleTierSweepImagesRetryExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Flags(tc.rule)
			if !ok {
				t.Fatalf("Flags(%q) has no row", tc.rule)
			}
			if got != want {
				t.Errorf("Flags(%q) = %+v, want the first failure's class %+v", tc.rule, got, want)
			}
			if !got.Wake || got.Handled != HandledOperatorAction {
				t.Errorf("Flags(%q) = %+v, want operator_action and wake", tc.rule, got)
			}
		})
	}
}

// TestTierSweepImagesRetryExhaustedIsTheOverseers pins the images-retry rows to
// the overseer, one row per rule: the images workflow is the town's own to
// re-run or fix before a person is woken, the same as its first failure. Each
// row is read from the table directly because RuleOwner answers the same value
// by family fallback, which would hide a deleted row from every test that
// reads only the answer (gt-j6cwqr).
func TestTierSweepImagesRetryExhaustedIsTheOverseers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rule string
	}{
		{"images failed", RuleImagesFailed},
		{"retries exhausted", RuleTierSweepImagesRetryExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ruleOwners[tc.rule]
			if !ok {
				t.Fatalf("the owner table has no row for %q", tc.rule)
			}
			if got != OwnerOverseer {
				t.Errorf("the owner table gives %q to %q, want %q", tc.rule, got, OwnerOverseer)
			}
			if owner := RuleOwner(tc.rule); owner != got {
				t.Errorf("RuleOwner(%q) = %q, want the table's %q", tc.rule, owner, got)
			}
		})
	}
}
