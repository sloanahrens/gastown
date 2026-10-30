package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/landworker"
)

// queueLandingReads counts the landing column's reads of one rig in the fake
// store's call log.
func queueLandingReads(fake *queueStores, rig string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := 0
	for _, call := range fake.calls {
		if call == rig+"/waiting_to_land" {
			n++
		}
	}
	return n
}

// A rig whose landing worker published a fresh count is answered from it, with
// no board read for that rig (gt-hw6ih).
func TestSourceQueueLandingColumnReadsThePublishedCount(t *testing.T) {
	t.Parallel()
	fake := &queueStores{rows: map[string]int{
		"alpha/waiting_to_land": 3, "beta/waiting_to_land": 1,
		"alpha/ready": 2, "beta/ready": 2,
	}}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.LandingQueue = func(time.Time) map[string]int { return map[string]int{"alpha": 4, "beta": 2} }
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"waiting_to_land": 6, "ready": 4})
	if n := queueLandingReads(fake, "alpha") + queueLandingReads(fake, "beta"); n != 0 {
		t.Errorf("the landing board was read %d times while the view answered it; want none", n)
	}
}

// A rig the view leaves out — never published, or published too long ago — is
// read from its own store, and only that rig pays for it.
func TestSourceQueueLandingColumnReadsTheRigTheViewLeavesOut(t *testing.T) {
	t.Parallel()
	fake := &queueStores{rows: map[string]int{
		"alpha/waiting_to_land": 3, "beta/waiting_to_land": 5,
	}}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.LandingQueue = func(time.Time) map[string]int { return map[string]int{"alpha": 4} }
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"waiting_to_land": 9})
	if n := queueLandingReads(fake, "alpha"); n != 0 {
		t.Errorf("alpha's board was read %d times; the view answered it", n)
	}
	if n := queueLandingReads(fake, "beta"); n != 1 {
		t.Errorf("beta's board was read %d times; want the one read the view left to it", n)
	}
}

// The bound is the view's: a count exactly at LandingQueueMaxAge answers, and a
// second older leaves the column to the store.
func TestSourceQueueLandingColumnFallsBackPastTheViewsBound(t *testing.T) {
	t.Parallel()
	clock := fixedNow()
	view := landworker.NewLandingQueueView(func() time.Time { return clock })
	view.Publish("alpha", 4)
	fake := &queueStores{rows: map[string]int{"alpha/waiting_to_land": 3}}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake, nil)
	env.LandingQueue = view.Counts
	col := queueLandingColumn()

	env.Now = func() time.Time { return fixedNow().Add(landworker.LandingQueueMaxAge) }
	if n, ok := queueSumCount(context.Background(), env, []string{"alpha"}, col); !ok || n != 4 {
		t.Fatalf("count at the bound = %d (ok %v); want the published 4", n, ok)
	}
	if n := queueLandingReads(fake, "alpha"); n != 0 {
		t.Fatalf("alpha's board was read %d times at the bound; want none", n)
	}

	env.Now = func() time.Time { return fixedNow().Add(landworker.LandingQueueMaxAge + time.Second) }
	if n, ok := queueSumCount(context.Background(), env, []string{"alpha"}, col); !ok || n != 3 {
		t.Fatalf("count past the bound = %d (ok %v); want the board's 3", n, ok)
	}
	if n := queueLandingReads(fake, "alpha"); n != 1 {
		t.Fatalf("alpha's board was read %d times past the bound; want one", n)
	}
}

// The view answers one rig and the store another's read fails: the column is
// nil, because a sum of the two would read as a queue that shrank.
func TestSourceQueueLandingColumnNilWhenAFallbackRigFails(t *testing.T) {
	t.Parallel()
	fake := &queueStores{
		rows: map[string]int{"alpha/waiting_to_land": 3, "beta/waiting_to_land": 5},
		fail: map[string]bool{"beta/waiting_to_land": true},
	}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.LandingQueue = func(time.Time) map[string]int { return map[string]int{"alpha": 4} }
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"waiting_to_land": nil})
}

// queueLandingLists is how many landing-board lists the census counted from the
// landing column since its last reset, which the snapshot also clears.
func queueLandingLists() int {
	n := 0
	for _, row := range beads.CensusSnapshotAndReset() {
		if row.Verb == "list" && row.Caller == "metrics.queueLandingColumn" {
			n += row.Count
		}
	}
	return n
}

// queueLandingPage is bd's side of a landing-board list: the allow-stale probe
// answers a version and every list an empty page, because this test counts the
// calls and not the beads (gt-hw6ih).
func queueLandingPage(_ string, args []string) ([]byte, []byte, error) {
	if len(args) > 0 && args[len(args)-1] == "version" {
		return []byte("bd version 1.2.2\n"), nil, nil
	}
	return []byte("[]"), nil, nil
}

// Ten samples of the landing board with fresh counts published make no bd call,
// and the same ten without one make the one list per rig per sample the sampler
// paid before gt-hw6ih. The census is process-wide, so this test runs alone.
func TestQueueLandingColumnMakesNoBdCallForAFreshView(t *testing.T) {
	beads.CensusSnapshotAndReset()
	t.Cleanup(func() { beads.CensusSnapshotAndReset() })

	view := landworker.NewLandingQueueView(fixedNow)
	view.Publish("alpha", 4)
	view.Publish("beta", 2)
	env := seamEnv(t, queueLandingPage)
	env.LandingQueue = view.Counts
	col := queueLandingColumn()

	for i := 0; i < 10; i++ {
		n, ok := queueSumCount(context.Background(), env, []string{"alpha", "beta"}, col)
		if !ok || n != 6 {
			t.Fatalf("sample %d = %d (ok %v); want the published 6", i, n, ok)
		}
	}
	if calls := queueLandingLists(); calls != 0 {
		t.Fatalf("a fresh view listed the board %d time(s); want no bd call", calls)
	}

	env.LandingQueue = nil
	for i := 0; i < 10; i++ {
		if _, ok := queueSumCount(context.Background(), env, []string{"alpha", "beta"}, col); !ok {
			t.Fatalf("sample %d with no published count failed", i)
		}
	}
	if calls := queueLandingLists(); calls != 20 {
		t.Fatalf("list calls = %d with no published count; want one per rig per sample (20)", calls)
	}
}
