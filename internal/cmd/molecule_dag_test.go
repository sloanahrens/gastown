package cmd

import (
	"reflect"
	"testing"
)

// findCriticalPath starts only from the tier-0 roots in dag.TierGroups, so
// these fixtures set each node's tier by hand instead of calling computeTiers;
// computeTiers itself stops at the first cycle and leaves the cyclic nodes
// without a tier.
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
