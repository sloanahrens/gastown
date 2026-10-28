package sling

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestAdmit pins which blocker edges refuse a dispatch and what each refusal
// names. The deferred rows answer the admission contract directly — Admit
// refuses a bead waiting on a deferred blocker — so they go red if the rule
// they delegate to ever stops counting deferred as unresolved (gt-o70a9).
func TestAdmit(t *testing.T) {
	t.Parallel()
	edge := func(id, status, depType string) beads.IssueDep {
		return beads.IssueDep{ID: id, Status: status, DependencyType: depType}
	}
	tests := []struct {
		name     string
		blockers []beads.IssueDep
		want     []string
	}{
		{
			name:     "no blockers",
			blockers: nil,
		},
		{
			name:     "empty list",
			blockers: []beads.IssueDep{},
		},
		{
			name: "all closed",
			blockers: []beads.IssueDep{
				edge("gt-a", "closed", "blocks"),
				edge("gt-b", "closed", "waits-for"),
			},
		},
		{
			name:     "open blocker refuses",
			blockers: []beads.IssueDep{edge("gt-open", "open", "blocks")},
			want:     []string{"gt-open (open)"},
		},
		{
			name:     "deferred blocker refuses",
			blockers: []beads.IssueDep{edge("gt-deferred", "deferred", "blocks")},
			want:     []string{"gt-deferred (deferred)"},
		},
		{
			name: "mixed list reports every unresolved one",
			blockers: []beads.IssueDep{
				edge("gt-open", "open", "blocks"),
				edge("gt-closed", "closed", "blocks"),
				edge("gt-deferred", "deferred", "waits-for"),
			},
			want: []string{"gt-open (open)", "gt-deferred (deferred)"},
		},
		{
			name: "non-blocking edge does not refuse",
			blockers: []beads.IssueDep{
				edge("gt-parent", "open", "parent-child"),
				edge("gt-tracked", "open", "tracks"),
			},
		},
		{
			name:     "unread dependency type refuses",
			blockers: []beads.IssueDep{{ID: "gt-a", Status: "open"}},
			want:     []string{"gt-a (open, dependency type unread)"},
		},
		{
			name:     "unreadable id on an open edge refuses",
			blockers: []beads.IssueDep{edge("", "open", "blocks")},
			want:     []string{"a blocker with an unreadable id (open)"},
		},
		{
			name:     "unreadable id on a closed edge does not refuse",
			blockers: []beads.IssueDep{edge("", "closed", "blocks")},
		},
		{
			name:     "unread status refuses",
			blockers: []beads.IssueDep{edge("gt-a", "", "blocks")},
			want:     []string{"gt-a (status unread)"},
		},
		{
			name:     "external id reads as its raw id",
			blockers: []beads.IssueDep{edge("external:gt:gt-a", "open", "blocks")},
			want:     []string{"gt-a (open)"},
		},
		{
			name: "one id shared by two edges names one reason",
			blockers: []beads.IssueDep{
				edge("gt-a", "open", "blocks"),
				edge("gt-a", "open", "waits-for"),
			},
			want: []string{"gt-a (open)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Admit(tt.blockers); !slices.Equal(got, tt.want) {
				t.Errorf("Admit() = %q, want %q", got, tt.want)
			}
		})
	}
}
