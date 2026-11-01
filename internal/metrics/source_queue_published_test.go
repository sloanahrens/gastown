package metrics

import (
	"context"
	"fmt"
	"strings"
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
	if n, ok := queueSumCount(context.Background(), env, queueTestRigList("alpha"), col); !ok || n != 4 {
		t.Fatalf("count at the bound = %d (ok %v); want the published 4", n, ok)
	}
	if n := queueLandingReads(fake, "alpha"); n != 0 {
		t.Fatalf("alpha's board was read %d times at the bound; want none", n)
	}

	env.Now = func() time.Time { return fixedNow().Add(landworker.LandingQueueMaxAge + time.Second) }
	if n, ok := queueSumCount(context.Background(), env, queueTestRigList("alpha"), col); !ok || n != 3 {
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
// paid before gt-hw6ih.
//
//testpolicy:allow parallel - the census counts bd calls process-wide, so another test making one mid-run would read as this test's
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
		n, ok := queueSumCount(context.Background(), env, queueTestRigList("alpha", "beta"), col)
		if !ok || n != 6 {
			t.Fatalf("sample %d = %d (ok %v); want the published 6", i, n, ok)
		}
	}
	if calls := queueLandingLists(); calls != 0 {
		t.Fatalf("a fresh view listed the board %d time(s); want no bd call", calls)
	}

	env.LandingQueue = nil
	for i := 0; i < 10; i++ {
		if _, ok := queueSumCount(context.Background(), env, queueTestRigList("alpha", "beta"), col); !ok {
			t.Fatalf("sample %d with no published count failed", i)
		}
	}
	if calls := queueLandingLists(); calls != 20 {
		t.Fatalf("list calls = %d with no published count; want one per rig per sample (20)", calls)
	}
}

// A rig the landing worker does not pass publishes no count, so a burst sample
// lists its board; every sample inside BurstSlowInterval of that read answers
// from the kept count instead, and the sample after the interval lists again
// (gt-pg5rb).
func TestWaitingToLandReusesABoardReadForOneMinute(t *testing.T) {
	t.Parallel()
	fake := &queueStores{rows: map[string]int{
		"alpha/waiting_to_land": 3, "beta/waiting_to_land": 2,
	}}
	now := fixedNow()
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.Now = func() time.Time { return now }
	env.BurstBoard = NewBurstBoardCache(nil)
	sample := func() *int {
		t.Helper()
		got := burstLandingQueue(context.Background(), env)
		if got == nil {
			t.Fatalf("the landing queue is nil; want both boards' sum")
		}
		return got
	}
	listings := func() int { return queueLandingReads(fake, "alpha") + queueLandingReads(fake, "beta") }

	for i := 0; i < 4; i++ {
		if got := *sample(); got != 5 {
			t.Fatalf("sample %d = %d; want 5", i, got)
		}
		now = now.Add(BurstFastInterval)
	}
	if n := listings(); n != 2 {
		t.Fatalf("four samples inside one minute listed the boards %d time(s); want the first sample's 2", n)
	}

	now = now.Add(BurstSlowInterval)
	if got := *sample(); got != 5 {
		t.Fatalf("the sample past the minute = %d; want 5", got)
	}
	if n := listings(); n != 4 {
		t.Fatalf("the sample past the minute took the listings to %d; want 4", n)
	}
}

// A fresh published count answers its rig even when a sample kept a board count
// for it: the published number is the landing worker's own read of the board.
func TestWaitingToLandPrefersAFreshPublishedCount(t *testing.T) {
	t.Parallel()
	fake := &queueStores{rows: map[string]int{
		"alpha/waiting_to_land": 3, "beta/waiting_to_land": 2,
	}}
	now := fixedNow()
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.Now = func() time.Time { return now }
	env.BurstBoard = NewBurstBoardCache(nil)
	env.LandingQueue = func(time.Time) map[string]int { return map[string]int{"alpha": 7} }

	first := burstLandingQueue(context.Background(), env)
	if first == nil || *first != 9 {
		t.Fatalf("the first sample = %v; want the published 7 plus the listed 2", first)
	}
	if n := queueLandingReads(fake, "beta"); n != 1 {
		t.Fatalf("beta's board was listed %d time(s); want the one read that kept its count", n)
	}

	now = now.Add(BurstFastInterval)
	env.LandingQueue = func(time.Time) map[string]int { return map[string]int{"alpha": 7, "beta": 4} }
	second := burstLandingQueue(context.Background(), env)
	if second == nil || *second != 11 {
		t.Fatalf("the second sample = %v; want the published 7 and 4", second)
	}
	if n := queueLandingReads(fake, "alpha"); n != 0 {
		t.Errorf("alpha's board was listed %d time(s); the published count answered it", n)
	}
	if n := queueLandingReads(fake, "beta"); n != 1 {
		t.Errorf("beta's board was listed %d time(s); the published count answered over the kept count", n)
	}
}

// A failed board read is not kept: the next sample lists that rig again and the
// column stays nil until a read answers.
func TestWaitingToLandDoesNotKeepAFailedRead(t *testing.T) {
	t.Parallel()
	fake := &queueStores{
		rows: map[string]int{"alpha/waiting_to_land": 3, "beta/waiting_to_land": 2},
		fail: map[string]bool{"beta/waiting_to_land": true},
	}
	now := fixedNow()
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.Now = func() time.Time { return now }
	env.BurstBoard = NewBurstBoardCache(nil)

	for i := 0; i < 2; i++ {
		if got := burstLandingQueue(context.Background(), env); got != nil {
			t.Fatalf("sample %d = %d; want nil while beta's board cannot be read", i, *got)
		}
		now = now.Add(BurstFastInterval)
	}
	if n := queueLandingReads(fake, "beta"); n != 2 {
		t.Fatalf("beta's board was listed %d time(s) over two samples; want 2, since a failed read is not kept", n)
	}

	fake.mu.Lock()
	fake.fail["beta/waiting_to_land"] = false
	fake.mu.Unlock()
	answered := burstLandingQueue(context.Background(), env)
	if answered == nil || *answered != 5 {
		t.Fatalf("the sample after beta's read answered = %v; want 5", answered)
	}
	if n := queueLandingReads(fake, "beta"); n != 3 {
		t.Fatalf("beta's board was listed %d time(s); want the third read that answered", n)
	}
}

// The sample names, in one line a minute, the rigs whose board it had to list,
// so the census's list count can be matched to rigs. A published rig is not
// named, and a read that fails is listed on every fast sample but still named
// once, which is the case the once-a-minute line exists for.
func TestWaitingToLandNamesTheListedRigsOnceAMinute(t *testing.T) {
	t.Parallel()
	fake := &queueStores{
		rows: map[string]int{"alpha/waiting_to_land": 3, "beta/waiting_to_land": 2},
		fail: map[string]bool{"beta/waiting_to_land": true},
	}
	now := fixedNow()
	var lines []string
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	env.Now = func() time.Time { return now }
	env.BurstBoard = NewBurstBoardCache(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	env.LandingQueue = func(time.Time) map[string]int { return map[string]int{"alpha": 3} }

	for i := 0; i < 4; i++ {
		if got := burstLandingQueue(context.Background(), env); got != nil {
			t.Fatalf("sample %d = %d; want nil while beta's board cannot be read", i, *got)
		}
		now = now.Add(BurstFastInterval)
	}
	if n := queueLandingReads(fake, "beta"); n != 4 {
		t.Fatalf("beta's board was listed %d time(s) over four fast samples; want 4, since a failed read is not kept", n)
	}
	if len(lines) != 1 {
		t.Fatalf("four samples inside one minute wrote %d line(s); want one", len(lines))
	}
	if got := lines[0]; !strings.Contains(got, "beta") || strings.Contains(got, "alpha") {
		t.Errorf("the line = %q; want beta named and alpha left out, since its published count answered it", got)
	}

	now = now.Add(BurstSlowInterval)
	burstLandingQueue(context.Background(), env)
	if len(lines) != 2 {
		t.Fatalf("the sample past the minute took the lines to %d; want a second one", len(lines))
	}
}
