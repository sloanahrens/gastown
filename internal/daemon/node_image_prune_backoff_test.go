package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A fresh id is allowed; an id whose last failure was 29 minutes ago is not and
// one whose last failure was 30 minutes ago is; three failures bar the id for
// good; the empty id is never allowed. The window runs from the last of two
// failures, and two failures are still under the limit.
func TestNodePruneBackoffDecides(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		id       string
		failures []time.Time
		want     bool
	}{
		{"fresh", "run-1", nil, true},
		{"29 minutes", "run-2", []time.Time{now.Add(-29 * time.Minute)}, false},
		{"30 minutes", "run-3", []time.Time{now.Add(-30 * time.Minute)}, true},
		{"three failures", "run-4", []time.Time{now.Add(-24 * time.Hour), now.Add(-24 * time.Hour), now.Add(-24 * time.Hour)}, false},
		{"two failures, 29 minutes after the last", "run-5", []time.Time{now.Add(-59 * time.Minute), now.Add(-29 * time.Minute)}, false},
		{"two failures, 30 minutes after the last", "run-6", []time.Time{now.Add(-60 * time.Minute), now.Add(-30 * time.Minute)}, true},
		{"empty id", "", []time.Time{now.Add(-24 * time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b nodePruneBackoff
			for _, at := range tt.failures {
				b.Failed(tt.id, at)
			}
			if got := b.Allowed(tt.id, now); got != tt.want {
				t.Errorf("Allowed(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// The map holds at most nodePruneBackoffEntries run ids: when a new id fails
// into a full map, the id whose last failure is oldest is dropped and may be
// tried again, while a newer id stays barred (gt-42832l).
func TestNodePruneBackoffDropsTheOldestWhenFull(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var b nodePruneBackoff
	for i := 0; i < nodePruneBackoffMax; i++ {
		b.Failed("oldest", now)
		b.Failed("newest", now.Add(time.Minute))
	}
	for i := 0; i < nodePruneBackoffEntries-2; i++ {
		b.Failed(fmt.Sprintf("filler-%d", i), now.Add(time.Duration(i+2)*time.Minute))
	}
	b.Failed("newest", now.Add(time.Hour))
	if b.Allowed("oldest", now.Add(time.Hour)) {
		t.Fatal("a repeat failure of an id already in a full map dropped the oldest entry")
	}
	b.Failed("one-more", now.Add(time.Hour))
	if !b.Allowed("oldest", now.Add(time.Hour)) {
		t.Fatal("the run id whose last failure was oldest was not dropped from a full map")
	}
	if b.Allowed("newest", now.Add(time.Hour)) {
		t.Fatal("a newer run id was dropped from a full map")
	}
}

// Failed and Allowed run from several goroutines at once for one shared id and
// for several distinct ids, so the race detector sees an entries map read and
// written without its lock.
func TestNodePruneBackoffIsRaceClean(t *testing.T) {
	t.Parallel()
	var b nodePruneBackoff
	ids := []string{"shared", "run-a", "run-b", "run-c", "run-d"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				for _, id := range ids {
					b.Failed(id, time.Time{})
					b.Allowed(id, time.Time{})
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	awaitNodePruneFlight(t, done, "every goroutine to finish calling Failed and Allowed")
}
