package attention

import "testing"

// TestImagesUnknownItemRaisesItsRule: a green commit whose run list could not be
// read resolves to images.unknown, which is queued for the overseer's next look
// and belongs to it. A rule with no row would leave the commit looking clean;
// a rule nobody owns is a queue row with no reader (gt-3vafwr).
func TestImagesUnknownItemRaisesItsRule(t *testing.T) {
	t.Parallel()
	it := Item{Kind: KindImagesUnknown, Key: "images-unknown:gastown:0123456789ab"}
	rule := RuleFor(it)
	if rule != RuleImagesUnknown {
		t.Fatalf("RuleFor(images-unknown item) = %q, want %q", rule, RuleImagesUnknown)
	}
	f, ok := Flags(rule)
	if !ok || f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %v; want operator_action without wake", rule, f, ok)
	}
	if owner := RuleOwner(rule); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", rule, owner, OwnerOverseer)
	}
}
