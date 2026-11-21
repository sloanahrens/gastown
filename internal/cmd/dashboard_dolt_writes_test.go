package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The actors' write deltas between two reads land in the buckets under the
// actor's name at the read's time: the first read adds nothing, a counter that
// went down (the gateway restarted) adds the new count, and an actor first seen
// on a later read adds its count.
func TestDoltWriteDeltasLandInTheBucketsByActor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var buckets dashboard.Buckets
	var w doltWriteDeltas
	read := func(step int, reads map[string]uint64, want map[string]int) {
		t.Helper()
		w.add(&buckets, now, reads)
		for actor, n := range want {
			series := buckets.Series(now, actor)
			if got := series[len(series)-1]; got != n {
				t.Errorf("step %d: bucket of %q holds %d, want %d", step, actor, got, n)
			}
		}
	}
	read(1, map[string]uint64{"a": 10, "b": 0}, map[string]int{"a": 0, "b": 0})
	read(2, map[string]uint64{"a": 70, "b": 5}, map[string]int{"a": 60, "b": 5})
	read(3, map[string]uint64{"a": 3, "b": 5}, map[string]int{"a": 63, "b": 5})
	read(4, map[string]uint64{"a": 3, "b": 5, "(other)": 2}, map[string]int{"a": 63, "b": 5, "(other)": 2})
	read(5, map[string]uint64{"a": 3, "b": 5, "(other)": 2}, map[string]int{"a": 63, "b": 5, "(other)": 2})
}
