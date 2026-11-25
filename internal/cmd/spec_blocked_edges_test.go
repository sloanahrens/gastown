package cmd

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
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

func TestInheritedFromRigs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		perRig map[string][]*beads.Issue
		want   map[string]specdispatch.Inherited
	}{
		{
			name: "ready blocker on no list takes the P0 of the blocked bead it holds up",
			perRig: map[string][]*beads.Issue{
				"gt": {{ID: "gt-b", Priority: 0, BlockedBy: []string{"gt-x"}}},
			},
			want: map[string]specdispatch.Inherited{
				"gt-x": {Priority: 0, From: "gt-b"},
			},
		},
		{
			name: "blockers in two rigs each take the priority of their own rig's dependent",
			perRig: map[string][]*beads.Issue{
				"gt": {{ID: "gt-b", Priority: 0, BlockedBy: []string{"gt-x"}}},
				"hq": {{ID: "hq-c", Priority: 1, BlockedBy: []string{"hq-y"}}},
			},
			want: map[string]specdispatch.Inherited{
				"gt-x": {Priority: 0, From: "gt-b"},
				"hq-y": {Priority: 1, From: "hq-c"},
			},
		},
		{
			name: "a blocker held up in two rigs keeps the better priority",
			perRig: map[string][]*beads.Issue{
				"gt": {{ID: "gt-b", Priority: 2, BlockedBy: []string{"gt-x"}}},
				"hq": {{ID: "hq-c", Priority: 0, BlockedBy: []string{"gt-x"}}},
			},
			want: map[string]specdispatch.Inherited{
				"gt-x": {Priority: 0, From: "hq-c"},
			},
		},
		{
			name: "a blocker that already outranks what it holds up gets no entry",
			perRig: map[string][]*beads.Issue{
				"gt": {
					{ID: "gt-x", Priority: 0},
					{ID: "gt-b", Priority: 2, BlockedBy: []string{"gt-x"}},
				},
			},
			want: map[string]specdispatch.Inherited{},
		},
		{
			name: "a blocker that is itself blocked passes its dependent's priority down the chain",
			perRig: map[string][]*beads.Issue{
				"gt": {
					{ID: "gt-x", Priority: 1, BlockedBy: []string{"gt-w"}},
					{ID: "gt-b", Priority: 2, BlockedBy: []string{"gt-x"}},
				},
			},
			want: map[string]specdispatch.Inherited{
				"gt-w": {Priority: 1, From: "gt-x"},
			},
		},
		{
			name: "a blocked issue with no blocked_by gives no entry",
			perRig: map[string][]*beads.Issue{
				"gt": {{ID: "gt-d", Priority: 0}},
			},
			want: map[string]specdispatch.Inherited{},
		},
		{
			name: "a nil issue is skipped and the next issue still counts",
			perRig: map[string][]*beads.Issue{
				"gt": {nil, {ID: "gt-b", Priority: 1, BlockedBy: []string{"gt-x"}}},
			},
			want: map[string]specdispatch.Inherited{
				"gt-x": {Priority: 1, From: "gt-b"},
			},
		},
		{
			name:   "no rigs gives an empty non-nil map",
			perRig: nil,
			want:   map[string]specdispatch.Inherited{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := inheritedFromRigs(tc.perRig)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("inheritedFromRigs() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
