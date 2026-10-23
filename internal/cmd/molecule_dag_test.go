package cmd

import (
	"reflect"
	"testing"
)

// findCriticalPath starts only from the tier-0 roots in dag.TierGroups, so
// these fixtures set each node's tier by hand instead of calling computeTiers,
// so the cycle sits behind a tier-0 root; computeTiers puts a cycle's nodes in
// a trailing tier of their own.
func TestFindCriticalPathSurvivesADependencyCycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dag  *DAGInfo
		want []string
	}{
		{
			name: "two-step cycle reached from the tier-0 step",
			dag: &DAGInfo{
				TotalNodes: 2,
				TierGroups: [][]string{{"gt-a"}, {"gt-b"}},
				Nodes: map[string]*DAGNode{
					"gt-a": {ID: "gt-a", Tier: 0, Dependents: []string{"gt-b"}},
					"gt-b": {ID: "gt-b", Tier: 1, Dependencies: []string{"gt-a"}, Dependents: []string{"gt-a"}},
				},
			},
			want: []string{"gt-a", "gt-b"},
		},
		{
			name: "three-step cycle closes at the node that opened the walk",
			dag: &DAGInfo{
				TotalNodes: 3,
				TierGroups: [][]string{{"gt-a"}, {"gt-b"}, {"gt-c"}},
				Nodes: map[string]*DAGNode{
					"gt-a": {ID: "gt-a", Tier: 0, Dependents: []string{"gt-b"}},
					"gt-b": {ID: "gt-b", Tier: 1, Dependencies: []string{"gt-a"}, Dependents: []string{"gt-c"}},
					"gt-c": {ID: "gt-c", Tier: 2, Dependencies: []string{"gt-b"}, Dependents: []string{"gt-a"}},
				},
			},
			want: []string{"gt-a", "gt-b", "gt-c"},
		},
		{
			name: "acyclic DAG still takes the longest branch",
			dag: &DAGInfo{
				TotalNodes: 4,
				TierGroups: [][]string{{"gt-a"}, {"gt-b", "gt-c"}, {"gt-d"}},
				Nodes: map[string]*DAGNode{
					"gt-a": {ID: "gt-a", Tier: 0, Dependents: []string{"gt-b", "gt-c"}},
					"gt-b": {ID: "gt-b", Tier: 1, Dependencies: []string{"gt-a"}},
					"gt-c": {ID: "gt-c", Tier: 1, Dependencies: []string{"gt-a"}, Dependents: []string{"gt-d"}},
					"gt-d": {ID: "gt-d", Tier: 2, Dependencies: []string{"gt-c"}},
				},
			},
			want: []string{"gt-a", "gt-c", "gt-d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := findCriticalPath(tt.dag); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("findCriticalPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

// tierMembers flattens dag.TierGroups and fails if a node is missing from them
// or appears twice, or if a node's Tier disagrees with the group holding it.
func tierMembers(t *testing.T, dag *DAGInfo) {
	t.Helper()

	seen := make(map[string]int)
	for tier, group := range dag.TierGroups {
		for _, id := range group {
			seen[id]++
			if got := dag.Nodes[id].Tier; got != tier {
				t.Errorf("node %s: Tier = %d, but it is in tier group %d", id, got, tier)
			}
		}
	}
	for id := range dag.Nodes {
		if seen[id] != 1 {
			t.Errorf("node %s appears in %d tier groups, want exactly 1", id, seen[id])
		}
	}
	if dag.Tiers != len(dag.TierGroups) {
		t.Errorf("Tiers = %d, want %d (len(TierGroups))", dag.Tiers, len(dag.TierGroups))
	}
}

func TestComputeTiersIncludesStepsWithExternalDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dag  *DAGInfo
		want [][]string
	}{
		{
			name: "dependent step also blocked by a bead outside the molecule",
			dag: &DAGInfo{
				TotalNodes: 2,
				Nodes: map[string]*DAGNode{
					"gt-a": {ID: "gt-a", Dependents: []string{"gt-b"}},
					"gt-b": {ID: "gt-b", Dependencies: []string{"gt-a", "gt-outside"}},
				},
			},
			want: [][]string{{"gt-a"}, {"gt-b"}},
		},
		{
			name: "root step blocked only by a bead outside the molecule",
			dag: &DAGInfo{
				TotalNodes: 1,
				Nodes: map[string]*DAGNode{
					"gt-a": {ID: "gt-a", Dependencies: []string{"gt-outside"}},
				},
			},
			want: [][]string{{"gt-a"}},
		},
		{
			name: "in-molecule cycle ends in one trailing tier",
			dag: &DAGInfo{
				TotalNodes: 4,
				Nodes: map[string]*DAGNode{
					"gt-a": {ID: "gt-a", Dependents: []string{"gt-b"}},
					"gt-b": {ID: "gt-b", Dependencies: []string{"gt-a", "gt-c"}, Dependents: []string{"gt-c"}},
					"gt-c": {ID: "gt-c", Dependencies: []string{"gt-b"}, Dependents: []string{"gt-b", "gt-d"}},
					"gt-d": {ID: "gt-d", Dependencies: []string{"gt-c"}},
				},
			},
			want: [][]string{{"gt-a"}, {"gt-b", "gt-c", "gt-d"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			computeTiers(tt.dag)
			if !reflect.DeepEqual(tt.dag.TierGroups, tt.want) {
				t.Errorf("TierGroups = %v, want %v", tt.dag.TierGroups, tt.want)
			}
			tierMembers(t, tt.dag)
		})
	}
}
