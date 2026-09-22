package cmd

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestFilterNonDispatchableBeads_ExcludesMailAndReports is the gt-b9wq
// acceptance criterion "no 'Re:' mail bead appears under Ready", for the filter
// that enforces it. Every row here is drawn as a Sling button, so a row is an
// invitation to dispatch.
func TestFilterNonDispatchableBeads_ExcludesMailAndReports(t *testing.T) {
	t.Parallel()

	issues := []*beads.Issue{
		{ID: "gt-real", Title: "Fix the flaky slot test", Type: "task", Priority: 2, Labels: []string{"gt:bug"}},
		{ID: "gt-real2", Title: "Add a --json flag", Type: "task", Priority: 3},
		{
			ID: "hq-6drv", Title: "Re: RESTART_POLECAT: gastown/ruby", Type: "task", Priority: 2,
			Labels: []string{"gt:message", "msg-type:reply", "read", "thread:thread-63e91151593e"},
		},
		{
			ID: "hq-g0gdg", Title: "POLECAT_DIED: 1 polecat(s) died with active work", Type: "task", Priority: 2,
			Labels: []string{"gt:message", "msg-type:", "delivery:acked"},
		},
		{ID: "hq-5f3", Title: "Compaction Report 2026-09-08", Type: "event", Priority: 2},
		{ID: "gt-zdcv", Title: "merge-slot", Type: "task", Priority: 0, Labels: []string{"gt:merge-slot"}},
		{ID: "gt-esc", Title: "[HIGH] dolt is unreachable", Type: "task", Priority: 1, Labels: []string{"gt:escalation"}},
	}

	filtered := filterNonDispatchableBeads(issues)

	if got, want := issueIDs(filtered), []string{"gt-real", "gt-real2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered IDs = %v, want %v", got, want)
	}
}

// TestFilterNonDispatchableBeads_KeepsAContainer guards the other direction: a
// family-level filter must not quietly become a priority or container filter.
// Epics and unstarted low-priority work are still rows someone can read, and
// dropping them here would change the shape of the board rather than what it
// counts as work.
func TestFilterNonDispatchableBeads_KeepsAContainer(t *testing.T) {
	t.Parallel()

	issues := []*beads.Issue{
		{ID: "gt-epic", Title: "de-flake the suite", Type: "epic", Priority: 1},
		{ID: "gt-low", Title: "someday, maybe", Type: "task", Priority: 4},
	}

	filtered := filterNonDispatchableBeads(issues)

	if got, want := issueIDs(filtered), []string{"gt-epic", "gt-low"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered IDs = %v, want %v", got, want)
	}
}

// TestFilterNonDispatchableBeads_AgreesWithDispatchCheck pins the shared
// definition to its other caller in the same package. The invariant is one-way:
// anything the board drops, the patrol must also refuse to count — a family the
// patrol calls work but the board hides is the drift behind gt-b9wq. The
// reverse does not hold, and must not: the patrol may suppress a notice the
// board still shows (see the test below).
func TestFilterNonDispatchableBeads_AgreesWithDispatchCheck(t *testing.T) {
	t.Parallel()

	// P2 so the priority bound in isActionableReadyBead cannot be the reason
	// the two disagree.
	issues := []*beads.Issue{
		{ID: "gt-work", Title: "real work", Type: "task", Priority: 2},
		{ID: "hq-mail", Title: "Re: something", Type: "task", Priority: 2, Labels: []string{"gt:message"}},
		{ID: "hq-report", Title: "Compaction Report 2026-09-08", Type: "event", Priority: 2},
		{ID: "gt-slot", Title: "merge-slot", Type: "task", Priority: 2, Labels: []string{"gt:merge-slot"}},
		{ID: "gt-handoff", Title: "session notes", Type: "task", Priority: 2, Labels: []string{"gt:handoff"}},
	}

	kept := map[string]bool{}
	for _, issue := range filterNonDispatchableBeads(issues) {
		kept[issue.ID] = true
	}

	for _, issue := range issues {
		if !kept[issue.ID] && isActionableReadyBead(issue) {
			t.Errorf("%s: ready list dropped it but dispatch-check counts it as actionable work",
				issue.ID)
		}
	}
}

// TestFilterNonDispatchableBeads_KeepsANoticeThePatrolSuppresses records the
// one place the two deliberately differ, so a later reader does not "fix" it
// by importing the patrol's list.
func TestFilterNonDispatchableBeads_KeepsANoticeThePatrolSuppresses(t *testing.T) {
	t.Parallel()

	// gt-59yz: a diagnosed, fixable bug against the main-branch test harness.
	// The patrol suppresses the prefix as a notification; the board must show
	// the work.
	notice := &beads.Issue{
		ID: "gt-59yz", Type: "bug", Priority: 2,
		Title: "main_branch_test: gastown killed at the 10m ctx timeout every run",
	}

	if got := filterNonDispatchableBeads([]*beads.Issue{notice}); len(got) != 1 {
		t.Errorf("ready list dropped %s; a filed bug is work even when its title reads like an alert", notice.ID)
	}
	if isActionableReadyBead(notice) {
		t.Errorf("dispatch-check should still suppress %s — the difference is intentional", notice.ID)
	}
}
