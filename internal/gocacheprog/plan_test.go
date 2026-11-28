package gocacheprog

import (
	"maps"
	"slices"
	"testing"
	"time"
)

// planEntry is an index entry with only the fields Plan reads.
func planEntry(output string, size int64, lastUse int) IndexEntry {
	return IndexEntry{OutputID: output, Size: size, LastUse: indexAt(lastUse)}
}

// planEntryUsed is an index entry whose last use is an exact time.
func planEntryUsed(output string, size int64, lastUse time.Time) IndexEntry {
	return IndexEntry{OutputID: output, Size: size, LastUse: lastUse}
}

// planNow is later than every last use in the table tests, so none is young.
var planNow = indexAt(0).Add(24 * time.Hour)

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
		protectFrom time.Time
		now         time.Time
		wantActions []string
		wantOutputs []string
		want        Summary
	}{
		{"under the limit evicts nothing", Index{"a": planEntry("x", 10, 1)}, 11, time.Time{}, planNow, nil, nil, Summary{Entries: 1, TotalBytes: 10}},
		{"exactly at the limit evicts nothing", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 20, 2)}, 30, time.Time{}, planNow, nil, nil, Summary{Entries: 2, TotalBytes: 30}},
		{"the least recently used entry goes first and stops the walk", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 20, 2)}, 25, time.Time{}, planNow, []string{"a"}, []string{"x"}, Summary{Entries: 2, TotalBytes: 30, EvictedEntries: 1, EvictedBytes: 10}},
		{"every entry goes when the limit is below every body", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 20, 2)}, 5, time.Time{}, planNow, []string{"a", "b"}, []string{"x", "y"}, Summary{Entries: 2, TotalBytes: 30, EvictedEntries: 2, EvictedBytes: 30}},
		{"a tie goes to the lower action id", Index{"b": planEntry("y", 10, 2), "a": planEntry("x", 10, 2)}, 10, time.Time{}, planNow, []string{"a"}, []string{"x"}, Summary{Entries: 2, TotalBytes: 20, EvictedEntries: 1, EvictedBytes: 10}},
		{"a shared body counts once and goes with its last entry", Index{"a": planEntry("x", 100, 1), "b": planEntry("x", 100, 2), "c": planEntry("y", 100, 3)}, 150, time.Time{}, planNow, []string{"a", "b"}, []string{"x"}, Summary{Entries: 3, TotalBytes: 200, EvictedEntries: 2, EvictedBytes: 100}},
		{"an entry whose shared body is still named leaves the body", Index{"a": planEntry("x", 100, 1), "b": planEntry("x", 100, 5), "c": planEntry("y", 100, 2)}, 150, time.Time{}, planNow, []string{"a", "c"}, []string{"y"}, Summary{Entries: 3, TotalBytes: 200, EvictedEntries: 2, EvictedBytes: 100}},
		{"an empty index evicts nothing", Index{}, 1, time.Time{}, planNow, nil, nil, Summary{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			before := maps.Clone(c.idx)
			actions, outputs, summary, err := Plan(c.idx, c.maxBytes, c.protectFrom, c.now)
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

func TestPlanProtectsEntriesUsedAtOrAfterProtectFrom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		idx         Index
		maxBytes    int64
		protectFrom time.Time
		wantActions []string
		wantOutputs []string
		want        Summary
	}{
		{"a zero protectFrom protects nothing", Index{"a": planEntry("x", 10, 1)}, 5, time.Time{}, []string{"a"}, []string{"x"}, Summary{Entries: 1, TotalBytes: 10, EvictedEntries: 1, EvictedBytes: 10}},
		{"an entry used before protectFrom is evicted", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 10, 3)}, 15, indexAt(3), []string{"a"}, []string{"x"}, Summary{Entries: 2, TotalBytes: 20, EvictedEntries: 1, EvictedBytes: 10}},
		{"an entry used exactly at protectFrom is kept", Index{"a": planEntry("x", 10, 2)}, 5, indexAt(2), nil, nil, Summary{Entries: 1, TotalBytes: 10, OverInUseBytes: 5}},
		{"an entry used after protectFrom is kept", Index{"a": planEntry("x", 10, 1), "b": planEntry("y", 10, 4)}, 15, indexAt(2), []string{"a"}, []string{"x"}, Summary{Entries: 2, TotalBytes: 20, EvictedEntries: 1, EvictedBytes: 10}},
		{"a body a protected entry names stays and the excess is reported", Index{"a": planEntry("x", 100, 1), "b": planEntry("x", 100, 3), "c": planEntry("y", 100, 2)}, 50, indexAt(3), []string{"a", "c"}, []string{"y"}, Summary{Entries: 3, TotalBytes: 200, EvictedEntries: 2, EvictedBytes: 100, OverInUseBytes: 50}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			actions, outputs, summary, err := Plan(c.idx, c.maxBytes, c.protectFrom, planNow)
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
		})
	}
}

func TestPlanYoungBytes(t *testing.T) {
	t.Parallel()
	now := indexAt(100000)
	youngFrom := now.Add(-youngWindow)
	cases := []struct {
		name     string
		idx      Index
		maxBytes int64
		want     int64
	}{
		{"a use exactly 75 minutes old counts", Index{"a": planEntryUsed("x", 10, youngFrom)}, 100, 10},
		{"a use 75 minutes and a second old does not count", Index{"a": planEntryUsed("x", 10, youngFrom.Add(-time.Second))}, 100, 0},
		{"a shared body counts once", Index{"a": planEntryUsed("x", 10, now), "b": planEntryUsed("x", 10, now)}, 100, 10},
		{"a body is young when any entry naming it was used recently", Index{"a": planEntryUsed("x", 10, youngFrom), "b": planEntryUsed("x", 10, youngFrom.Add(-time.Hour))}, 100, 10},
		{"young bytes describe the index before eviction", Index{"a": planEntryUsed("x", 10, now)}, 5, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, summary, err := Plan(c.idx, c.maxBytes, time.Time{}, now)
			if err != nil {
				t.Fatalf("Plan() error = %v, want none", err)
			}
			if summary.YoungBytes != c.want {
				t.Errorf("YoungBytes = %d, want %d", summary.YoungBytes, c.want)
			}
		})
	}
}

func TestPlanRefusesNonPositiveMaxBytes(t *testing.T) {
	t.Parallel()
	for _, maxBytes := range []int64{0, -1} {
		actions, outputs, summary, err := Plan(Index{"a": planEntry("x", 10, 1)}, maxBytes, time.Time{}, planNow)
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
	if _, _, _, err := Plan(idx, 1, time.Time{}, planNow); err == nil {
		t.Error("Plan() error = nil for one body named at two sizes, want an error")
	}
}
