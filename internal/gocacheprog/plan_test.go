package gocacheprog

import (
	"maps"
	"slices"
	"testing"
)

// planEntry is an index entry with only the fields Plan reads.
func planEntry(output string, size int64, lastUse int) IndexEntry {
	return IndexEntry{OutputID: output, Size: size, LastUse: indexAt(lastUse)}
}

// equalIDs reports whether got holds exactly want, in order.
func equalIDs(got [][]byte, want ...string) bool {
	return slices.EqualFunc(got, want, func(g []byte, w string) bool { return string(g) == w })
}

func TestPlanEvictsLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		idx         Index
		maxBytes    int64
		wantActions []string
		wantOutputs []string
		want        Summary
	}{
		{"under the limit evicts nothing", Index{"a": planEntry("x", 10, 1)}, 11, nil, nil, Summary{Entries: 1, TotalBytes: 10}},
		{"exactly at the limit evicts nothing", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 20, 2)}, 30, nil, nil, Summary{Entries: 2, TotalBytes: 30}},
		{"the least recently used entry goes first and stops the walk", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 20, 2)}, 25, []string{"a"}, []string{"x"}, Summary{Entries: 2, TotalBytes: 30, EvictedEntries: 1, EvictedBytes: 10}},
		{"every entry goes when the limit is below every body", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 20, 2)}, 5, []string{"a", "b"}, []string{"x", "y"}, Summary{Entries: 2, TotalBytes: 30, EvictedEntries: 2, EvictedBytes: 30}},
		{"a tie goes to the lower action id", Index{"b": planEntry("y", 10, 2), "a": planEntry("x", 10, 2)}, 10, []string{"a"}, []string{"x"}, Summary{Entries: 2, TotalBytes: 20, EvictedEntries: 1, EvictedBytes: 10}},
		{"a shared body counts once and goes with its last entry", Index{"a": planEntry("x", 100, 1), "b": planEntry("x", 100, 2), "c": planEntry("y", 100, 3)}, 150, []string{"a", "b"}, []string{"x"}, Summary{Entries: 3, TotalBytes: 200, EvictedEntries: 2, EvictedBytes: 100}},
		{"an entry whose shared body is still named leaves the body", Index{"a": planEntry("x", 100, 1), "b": planEntry("x", 100, 5), "c": planEntry("y", 100, 2)}, 150, []string{"a", "c"}, []string{"y"}, Summary{Entries: 3, TotalBytes: 200, EvictedEntries: 2, EvictedBytes: 100}},
		{"an empty index evicts nothing", Index{}, 1, nil, nil, Summary{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			before := maps.Clone(c.idx)
			actions, outputs, summary, err := Plan(c.idx, c.maxBytes)
			if err != nil {
				t.Fatalf("Plan() error = %v, want none", err)
			}
			if !equalIDs(actions, c.wantActions...) {
				t.Errorf("actions = %q, want %q", actions, c.wantActions)
			}
			if !equalIDs(outputs, c.wantOutputs...) {
				t.Errorf("outputs = %q, want %q", outputs, c.wantOutputs)
			}
			if summary != c.want {
				t.Errorf("summary = %+v, want %+v", summary, c.want)
			}
			if !sameIndex(c.idx, before) {
				t.Errorf("Plan changed its input index: %+v", c.idx)
			}
		})
	}
}

func TestPlanRefusesNonPositiveMaxBytes(t *testing.T) {
	t.Parallel()
	for _, maxBytes := range []int64{0, -1} {
		actions, outputs, summary, err := Plan(Index{"a": planEntry("x", 10, 1)}, maxBytes)
		if err == nil {
			t.Errorf("Plan(maxBytes=%d) error = nil, want an error", maxBytes)
		}
		if actions != nil || outputs != nil || summary != (Summary{}) {
			t.Errorf("Plan(maxBytes=%d) = %q, %q, %+v with the refusal, want no plan", maxBytes, actions, outputs, summary)
		}
	}
}

func TestPlanRefusesSharedBodySizesThatDisagree(t *testing.T) {
	t.Parallel()
	idx := Index{"a": planEntry("x", 10, 1), "b": planEntry("x", 11, 2)}
	if _, _, _, err := Plan(idx, 1); err == nil {
		t.Error("Plan() error = nil for one body named at two sizes, want an error")
	}
}
