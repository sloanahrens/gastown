package attention

import "testing"

// The owner table is one row per rule: a reader that draws the human and the
// overseer the same list filters it by this, so a rule with no row is a row
// nobody owns, and a rule the table holds but the flags table does not is a row
// that can never be reached (gt-71714).
func TestEveryRuleHasAnOwner(t *testing.T) {
	t.Parallel()

	for _, rule := range Rules() {
		if _, ok := ruleOwners[rule]; !ok {
			t.Errorf("the owner table has no row for %q, so nothing owns it", rule)
		}
	}
	for rule := range ruleOwners {
		if !contains(Rules(), rule) {
			t.Errorf("the owner table names rule %q, which no longer exists", rule)
		}
	}

	// The human's rules: a decision or an action only the person can take. Every
	// other row is the overseer's, which the loop below pins so a rule moving
	// between the two is a test failure rather than a quiet retag.
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
}

// The rows the table cannot decide are the overseer's: an escalation.open row
// carries nothing about who raised the escalation, and EscalationOwner is what
// reads that off the escalation itself, so the family row must not claim the
// human. A rule this binary does not know reads the same way, as a line written
// before the table existed (gt-71714).
func TestRuleOwnerFallsBackToTheOverseer(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{RuleEscalation, "something.new", ""} {
		if got := RuleOwner(rule); got != OwnerOverseer {
			t.Errorf("RuleOwner(%q) = %q, want the overseer default", rule, got)
		}
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
