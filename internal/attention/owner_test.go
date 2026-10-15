package attention

import "testing"

// The owner table is the one place that decides whether an item is the human's
// or an agent's, so every rule has to be accounted for: a new rule that falls
// through to the default is the human quietly never being told about it
// (gt-71714).
func TestEveryRuleHasAnOwner(t *testing.T) {
	t.Parallel()
	// The human's rules: a decision or an action only the person can take.
	sloan := map[string]bool{
		RuleQuestionNew:    true,
		RuleQuestionStale:  true,
		RuleOverseerSilent: true,
		RuleRejectedTwice:  true,
		RuleHostDiskWarn:   true,
		RuleHostDiskCrit:   true,
		RuleHostMemLow:     true,
		RuleHostDockerDown: true,
		RuleHostProbe:      true,
	}
	for _, rule := range Rules() {
		want := OwnerOverseer
		if sloan[rule] {
			want = OwnerSloan
		}
		if got := RuleOwner(rule); got != want {
			t.Errorf("RuleOwner(%q) = %q, want %q", rule, got, want)
		}
	}
	for rule := range sloan {
		if !contains(Rules(), rule) {
			t.Errorf("the owner table names rule %q, which no longer exists", rule)
		}
	}

	// An escalation is the human's only when the overseer cannot resolve it,
	// which the per-escalation rule decides, so the family default is the
	// overseer's.
	if got := RuleOwner(RuleEscalation); got != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q: an escalation.open row has no owner to judge", RuleEscalation, got)
	}
	if got := RuleOwner("something.new"); got != OwnerOverseer {
		t.Errorf("RuleOwner of an unknown rule = %q, want the overseer default", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
