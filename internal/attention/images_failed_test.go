package attention

import "testing"

// TestImagesFailedItemRaisesTheWakingRule: a green commit whose image build
// failed resolves to images.failed, which wakes the overseer for an operator
// action and belongs to it. A rule that fell to info would leave a commit that
// can never be rolled onto waiting until someone looked; a rule nobody owns is
// a queue row with no reader (gt-3vafwr).
func TestImagesFailedItemRaisesTheWakingRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindImagesFailed, Key: "images-failed:gastown:0123456789ab"}
	rule := RuleFor(it)
	if rule != RuleImagesFailed {
		t.Fatalf("RuleFor(images-failed item) = %q, want %q", rule, RuleImagesFailed)
	}
	f, ok := Flags(rule)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action and wake", rule, f, ok)
	}
	if owner := RuleOwner(rule); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", rule, owner, OwnerOverseer)
	}
}
