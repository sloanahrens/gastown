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
	operator := map[string]bool{
		RuleQuestionNew:    true,
		RuleQuestionStale:  true,
		RuleOverseerSilent: true,
		RuleHostDiskWarn:   true,
		RuleHostDiskCrit:   true,
		RuleHostMemLow:     true,
		RuleHostDockerDown: true,
		RuleHostProbe:      true,
	}
	for _, rule := range Rules() {
		want := OwnerOverseer
		if operator[rule] {
			want = OwnerOperator
		}
		if got := RuleOwner(rule); got != want {
			t.Errorf("RuleOwner(%q) = %q, want %q", rule, got, want)
		}
	}
}

// The owner's value is what the dashboard serves and the page's chip and badge
// read, so the constant is pinned to the word itself: a rename of the constant
// that left the value behind would pass every test that reads the tag by name
// and serve a reader a person where they expect the operator (gt-rujis).
func TestTheOperatorsOwnerIsTheWordOperator(t *testing.T) {
	t.Parallel()

	if OwnerOperator != "operator" {
		t.Errorf("OwnerOperator = %q, want %q", string(OwnerOperator), "operator")
	}
}

// A second rejection is routine rework, so it is the overseer's item, and it
// still wakes the overseer: moving the owner must not drop the wake or demote
// the rule to a record (gt-few2w).
func TestRejectedTwiceIsTheOverseersAndStillWakes(t *testing.T) {
	t.Parallel()

	if got := RuleOwner(RuleRejectedTwice); got != OwnerOverseer {
		t.Errorf("RuleOwner(%q) = %q, want %q", RuleRejectedTwice, got, OwnerOverseer)
	}
	f, ok := Flags(RuleRejectedTwice)
	if !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%q) = %+v, want an operator_action that wakes", RuleRejectedTwice, f)
	}
}

// A rule the table has no row for — one a newer daemon wrote — reads by its
// family: a host reading and an escalation belong to the human, and everything
// else to the overseer, as a line written before the table existed reads. The
// rows the table cannot decide stay the overseer's own: an escalation.open row
// carries nothing about who raised the escalation, and EscalationOwner reads
// that off the escalation itself, so the family's own row must not claim the
// human and the families below must not overrule it (gt-71714, gt-bi6w0).
func TestRuleOwnerReadsAnUnknownRuleByItsFamily(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		rule string
		want Owner
	}{
		{RuleEscalation, OwnerOverseer}, // the family's row, corrected per escalation
		{"escalation.unhandled_hours", OwnerOperator},
		{"host.disk_full", OwnerOperator},
		{"landing.new_rule", OwnerOverseer}, // landing.* is the town's own
		{"question.expired", OwnerOverseer}, // a question's owner is its state, which only a row states
		{"something.new", OwnerOverseer},
		{"", OwnerOverseer},
	} {
		if got := RuleOwner(tc.rule); got != tc.want {
			t.Errorf("RuleOwner(%q) = %q, want %q", tc.rule, got, tc.want)
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
