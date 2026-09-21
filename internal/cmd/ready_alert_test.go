package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestFilterIdentityBeads_ExcludesAlertRecords is the gt-vwry acceptance
// criterion "bd ready shows none of them by default", for the dashboard Ready
// list this filter feeds.
//
// The records that filled that list were alert artifacts: an escalation, the
// mail copy of one, and the repeats a producer minted per firing before alerts
// were keyed. None has an owner and none is actionable, but an urgent alert is
// a P0, so they outranked real work and read to the dispatcher as a queue of
// emergencies.
func TestFilterIdentityBeads_ExcludesAlertRecords(t *testing.T) {
	issues := []*beads.Issue{
		{ID: "gt-real", Title: "Fix the thing", Priority: 2, Labels: []string{"gt:task"}},
		{ID: "gt-real2", Title: "Another piece of work", Priority: 1, Labels: []string{"gt:bug"}},
		{ID: "hq-esc", Title: "Dolt: server unreachable", Priority: 0, Labels: []string{"gt:escalation", "severity:critical"}},
		{ID: "hq-mail", Title: "[HIGH] main_branch_test: failures", Priority: 1, Type: "task", Labels: []string{"gt:message", "gt:escalation", "msg-type:escalation"}},
		{ID: "hq-mail2", Title: "[HIGH] jsonl spike", Priority: 1, Labels: []string{"gt:message", "gt:escalation"}},
		{ID: "hq-fp", Title: "recurring alert", Priority: 1, Labels: []string{"escalation-fp:abc123"}},
		{ID: "hq-title", Title: "[CRITICAL] quality breach: sapphire (avg 0.42)", Priority: 0},
	}

	filtered := filterIdentityBeads(issues)

	kept := map[string]bool{}
	for _, issue := range filtered {
		kept[issue.ID] = true
	}
	for _, want := range []string{"gt-real", "gt-real2"} {
		if !kept[want] {
			t.Errorf("%s is real work and must survive the filter", want)
		}
	}
	for _, gone := range []string{"hq-esc", "hq-mail", "hq-mail2", "hq-fp", "hq-title"} {
		if kept[gone] {
			t.Errorf("%s is an alert record and must be dropped from the ready list", gone)
		}
	}
}

// TestFilterIdentityBeads_KeepsRealWorkSharingAnAlertLabel guards the other
// direction: the filter keys on labels that only escalations and their mail
// copies carry, so ordinary work must survive it — including work whose
// severity a human happened to tag.
func TestFilterIdentityBeads_KeepsRealWorkSharingAnAlertLabel(t *testing.T) {
	issues := []*beads.Issue{
		{ID: "gt-sev", Title: "Investigate high-severity crash", Priority: 1, Labels: []string{"gt:bug", "severity:high"}},
		{ID: "gt-msg", Title: "Reply to the witness", Priority: 2, Type: "task", Labels: []string{"gt:message", "msg-type:task"}},
		{ID: "gt-esc2", Title: "Handle the escalation", Priority: 2, Type: "task", Labels: []string{"gt:message", "msg-type:task", "from:gastown/witness"}},
	}

	filtered := filterIdentityBeads(issues)
	if len(filtered) != len(issues) {
		t.Fatalf("filtered %d of %d issues, want all kept: %+v", len(filtered), len(issues), filtered)
	}
}

func TestIsEscalationTitle(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"[HIGH] main_branch_test: main branch test failures:", true},
		{"[CRITICAL] Dolt: server unreachable", true},
		{"[MEDIUM] something", true},
		{"[LOW] something", true},
		{"Fix [HIGH] severity handling", false},
		{"[high] lowercase is not the envelope", false},
		{"[HIGH]no space", false},
		{"main_branch_test: failures", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isEscalationTitle(tt.title); got != tt.want {
			t.Errorf("isEscalationTitle(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}
