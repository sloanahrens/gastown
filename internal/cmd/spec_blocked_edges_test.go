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

func TestSpecMergeBlockedEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		perRig []blockedEdges
		want   blockedEdges
	}{
		{
			name:   "no rigs gives empty non-nil maps",
			perRig: nil,
			want: blockedEdges{
				Own:       map[string]int{},
				BlockedBy: map[string][]string{},
			},
		},
		{
			name: "two rigs with disjoint ids give the union",
			perRig: []blockedEdges{
				{Own: map[string]int{"gt-a": 1}, BlockedBy: map[string][]string{"gt-a": {"gt-x"}}},
				{Own: map[string]int{"hq-b": 0}, BlockedBy: map[string][]string{"hq-b": {"hq-y"}}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 1, "hq-b": 0},
				BlockedBy: map[string][]string{"gt-a": {"gt-x"}, "hq-b": {"hq-y"}},
			},
		},
		{
			name: "an id in both rigs keeps the better priority when the worse rig comes first",
			perRig: []blockedEdges{
				{Own: map[string]int{"gt-a": 3}, BlockedBy: map[string][]string{}},
				{Own: map[string]int{"gt-a": 1}, BlockedBy: map[string][]string{}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 1},
				BlockedBy: map[string][]string{},
			},
		},
		{
			name: "an id in both rigs keeps the better priority when the better rig comes first",
			perRig: []blockedEdges{
				{Own: map[string]int{"gt-a": 0}, BlockedBy: map[string][]string{}},
				{Own: map[string]int{"gt-a": 2}, BlockedBy: map[string][]string{}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 0},
				BlockedBy: map[string][]string{},
			},
		},
		{
			name: "a blocker repeated across rigs appears once",
			perRig: []blockedEdges{
				{Own: map[string]int{"gt-a": 1}, BlockedBy: map[string][]string{"gt-a": {"gt-x", "gt-y"}}},
				{Own: map[string]int{"gt-a": 1}, BlockedBy: map[string][]string{"gt-a": {"gt-y", "gt-z"}}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 1},
				BlockedBy: map[string][]string{"gt-a": {"gt-x", "gt-y", "gt-z"}},
			},
		},
		{
			name: "a blocker repeated within one rig appears once",
			perRig: []blockedEdges{
				{Own: map[string]int{"gt-a": 1}, BlockedBy: map[string][]string{"gt-a": {"gt-x", "gt-x"}}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 1},
				BlockedBy: map[string][]string{"gt-a": {"gt-x"}},
			},
		},
		{
			name: "an empty blocker list adds no edge",
			perRig: []blockedEdges{
				{Own: map[string]int{"gt-a": 2}, BlockedBy: map[string][]string{"gt-a": {}}},
			},
			want: blockedEdges{
				Own:       map[string]int{"gt-a": 2},
				BlockedBy: map[string][]string{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := specMergeBlockedEdges(tc.perRig)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("specMergeBlockedEdges() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
