package beads

import "testing"

func TestIsNonDispatchableBead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		issue *Issue
		want  bool
	}{
		{
			name:  "nil issue",
			issue: nil,
			want:  false,
		},
		{
			name:  "plain task",
			issue: &Issue{ID: "gt-1", Title: "fix the flaky test", Type: "task", Priority: 2},
			want:  false,
		},
		{
			name:  "bug",
			issue: &Issue{ID: "gt-2", Title: "crash on empty input", Type: "bug", Priority: 1, Labels: []string{"gt:bug"}},
			want:  false,
		},
		{
			// The reported case: a reply in a mail thread. Its beads type is
			// "task" (mail beads are not typed "message"), so the label is the
			// only signal that distinguishes it from work.
			name: "mail reply",
			issue: &Issue{
				ID:     "hq-6drv",
				Title:  "Re: RESTART_POLECAT: gastown/ruby",
				Type:   "task",
				Labels: []string{"gt:message", "msg-type:reply", "read", "thread:thread-63e91151593e"},
			},
			want: true,
		},
		{
			name: "delivery copy of a notification",
			issue: &Issue{
				ID:     "hq-g0e75",
				Title:  "POLECAT_DIED: 1 polecat(s) died with active work",
				Type:   "task",
				Labels: []string{"gt:message", "msg-type:", "delivery:acked"},
			},
			want: true,
		},
		{
			name:  "message type",
			issue: &Issue{ID: "hq-3", Title: "note to self", Type: "message"},
			want:  true,
		},
		{
			name:  "handoff type",
			issue: &Issue{ID: "gt-4", Title: "pick up where I left off", Type: "handoff"},
			want:  true,
		},
		{
			name:  "HANDOFF title without the label",
			issue: &Issue{ID: "gt-5", Title: "HANDOFF: polecat flint, step 3 of 8", Type: "task"},
			want:  true,
		},
		{
			name:  "handoff label",
			issue: &Issue{ID: "gt-6", Title: "session notes", Type: "task", Labels: []string{"gt:handoff"}},
			want:  true,
		},
		{
			// Event beads record that something happened; they carry a
			// --event-category, not an owner. Two of these sat in Ready at P2.
			name:  "compaction report",
			issue: &Issue{ID: "hq-5f3", Title: "Compaction Report 2026-09-08", Type: "event", Priority: 2},
			want:  true,
		},
		{
			name:  "compaction report that lost its event type",
			issue: &Issue{ID: "hq-7", Title: "Compaction Report 2026-09-08", Type: "task", Priority: 2},
			want:  true,
		},
		{
			name:  "reaper run report",
			issue: &Issue{ID: "hq-cwble", Title: "reaper-dog run 2026-09-19T17:18Z: reaped 27 wisps", Type: "event", Priority: 3},
			want:  true,
		},
		{
			name:  "merge slot",
			issue: &Issue{ID: "gt-zdcv", Title: "merge-slot", Type: "task", Priority: 0, Labels: []string{"gt:merge-slot"}},
			want:  true,
		},
		{
			name:  "agent bead",
			issue: &Issue{ID: "gt-gastown-polecat-flint", Title: "gt-gastown-polecat-flint", Type: "agent", Labels: []string{"gt:agent"}},
			want:  true,
		},
		{
			name:  "escalation by label",
			issue: &Issue{ID: "gt-8", Title: "disk full", Type: "task", Priority: 1, Labels: []string{"gt:escalation"}},
			want:  true,
		},
		{
			name:  "convoy",
			issue: &Issue{ID: "hq-cv-1", Title: "dashboard convoy", Type: "convoy", Labels: []string{"gt:convoy"}},
			want:  true,
		},
		{
			name:  "queue",
			issue: &Issue{ID: "gt-10", Title: "queue", Type: "task", Labels: []string{"gt:queue"}},
			want:  true,
		},
		{
			name:  "standing orders",
			issue: &Issue{ID: "gt-11", Title: "mayor standing orders", Type: "task", Labels: []string{"gt:standing-orders"}},
			want:  true,
		},
		{
			name:  "label case and padding",
			issue: &Issue{ID: "gt-12", Title: "agent-ish", Type: "task", Labels: []string{" GT:AGENT "}},
			want:  true,
		},
		{
			// A title that merely contains an alert word is still work; only
			// the exact envelope is matched.
			name:  "work mentioning an escalation",
			issue: &Issue{ID: "gt-13", Title: "Handle the escalation when severity is high", Type: "task", Priority: 2},
			want:  false,
		},
		{
			// The seat patrol suppresses this prefix as a notification, and it
			// reads like one. But a title is a pattern, not a type: a bug filed
			// against the harness carries the same prefix and is still fixable,
			// so a board that exists to show work must keep it.
			name: "bug filed against the main-branch test harness",
			issue: &Issue{
				ID:   "gt-59yz",
				Type: "bug", Priority: 2,
				Title: "main_branch_test: gastown killed at the 10m ctx timeout every run",
			},
			want: false,
		},
		{
			name:  "state collapse notice",
			issue: &Issue{ID: "gt-15", Title: "STATE_COLLAPSE gt-vahe closed, branch never merged", Type: "task", Priority: 0},
			want:  false,
		},
		{
			// Epics and their containers are a priority judgement, not a family
			// one: the caller decides whether a container is dispatchable.
			name:  "epic",
			issue: &Issue{ID: "gt-14", Title: "de-flake the suite", Type: "epic", Priority: 1},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNonDispatchableBead(tc.issue); got != tc.want {
				t.Errorf("IsNonDispatchableBead(%+v) = %v, want %v", tc.issue, got, tc.want)
			}
		})
	}
}
