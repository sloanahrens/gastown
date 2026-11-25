package cmd

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestSpecBlockedMaps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		issues []*beads.Issue
		want   blockedEdges
	}{
		{
			name: "two blocked issues with blockers give both maps",
			issues: []*beads.Issue{
				{ID: "gt-a", Priority: 1, BlockedBy: []string{"gt-x"}},
				{ID: "gt-b", Priority: 0, BlockedBy: []string{"gt-y", "gt-z"}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 1, "gt-b": 0},
				BlockedBy: map[string][]string{"gt-a": {"gt-x"}, "gt-b": {"gt-y", "gt-z"}},
			},
		},
		{
			name:   "issue with no blocked_by gives its own priority and no edge",
			issues: []*beads.Issue{{ID: "gt-c", Priority: 2}},
			want: blockedEdges{
				Own:       map[string]int{"gt-c": 2},
				BlockedBy: map[string][]string{},
			},
		},
		{
			name:   "nil slice gives empty non-nil maps",
			issues: nil,
			want: blockedEdges{
				Own:       map[string]int{},
				BlockedBy: map[string][]string{},
			},
		},
		{
			name:   "nil entry is skipped and the next issue is kept",
			issues: []*beads.Issue{nil, {ID: "gt-d", Priority: 3}},
			want: blockedEdges{
				Own:       map[string]int{"gt-d": 3},
				BlockedBy: map[string][]string{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := specBlockedMaps(tc.issues)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("specBlockedMaps() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
