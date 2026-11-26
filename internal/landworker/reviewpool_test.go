package landworker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// waitCtx bounds one wait in a test: a wait that outlives it fails the test by
// name instead of hanging until the package timeout. The deadline is not a
// sleep; every wait it bounds returns as soon as its event happens.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// recvBounded receives one value from ch, or fails the test when the test's
// bound (its one waitCtx) passes first.
func recvBounded[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("%s did not happen within the test's bound", what)
		var zero T
		return zero
	}
}

// poolIdle runs pool.Wait on its own goroutine. started closes when the
// goroutine begins; idle closes when Wait returns. The bound is the caller's
// recvBounded on idle, so a Wait that never returns fails the test by name.
func idleOf(pool *ReviewPool) <-chan struct{} {
	_, idle := poolIdle(pool)
	return idle
}

func poolIdle(pool *ReviewPool) (started, idle <-chan struct{}) {
	wait := pool.Wait
	st, id := make(chan struct{}), make(chan struct{})
	go func() {
		close(st)
		wait()
		close(id)
	}()
	return st, id
}

// fakeAheadReviewer records each review it is asked for and holds it until
// the test closes release, or the bead's own channel in perWork when it has
// one, or its bound passes. A review whose bead is in fail returns that error.
type fakeAheadReviewer struct {
	entered chan land.Work
	release chan struct{}
	perWork map[string]chan struct{}
	bound   <-chan struct{}
	fail    map[string]error

	mu    sync.Mutex
	calls []land.Work
}

func newFakeAheadReviewer(t *testing.T, ctx context.Context, fail map[string]error) *fakeAheadReviewer {
	t.Helper()
	return &fakeAheadReviewer{entered: make(chan land.Work, 8), release: make(chan struct{}), bound: ctx.Done(), fail: fail}
}

func (f *fakeAheadReviewer) ReviewAhead(_ context.Context, w land.Work) (land.AheadOutcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, w)
	f.mu.Unlock()
	f.entered <- w
	release := f.release
	if ch, ok := f.perWork[w.BeadID]; ok {
		release = ch
	}
	select {
	case <-release:
	case <-f.bound:
		return "", errors.New("review held past the test's bound")
	}
	if err := f.fail[w.BeadID]; err != nil {
		return "", err
	}
	return land.AheadSkipped, nil
}

// reviewed returns the works the reviewer was asked to review.
func (f *fakeAheadReviewer) reviewed() []land.Work {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]land.Work(nil), f.calls...)
}

// logRecorder collects the lines a pool logs.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func TestReviewPoolReviewsEachWorkOnce(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	// Room for both: the pool starts no more than Max reviews at once (gt-ginbeo).
	pool := &ReviewPool{Reviewer: rev, Max: 2}

	pool.Offer(context.Background(), []land.Work{
		{BeadID: "gt-first", Head: "1111111"},
		{BeadID: "gt-second", Head: "2222222"},
	})
	started := map[string]string{}
	for n := 0; n < 2; n++ {
		w := recvBounded(t, ctx, rev.entered, "a review start")
		started[w.BeadID] = w.Head
	}
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if want := map[string]string{"gt-first": "1111111", "gt-second": "2222222"}; !reflect.DeepEqual(started, want) {
		t.Errorf("reviews started %v, want one per work %v", started, want)
	}
	if n := len(rev.reviewed()); n != 2 {
		t.Errorf("reviewer called %d times, want 2", n)
	}
}

func TestReviewPoolWaitReturnsWhenIdle(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, map[string]error{"gt-bad": errors.New("om unreachable")})
	logs := &logRecorder{}
	pool := &ReviewPool{Reviewer: rev, Max: 1, Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-bad", Head: "3333333"}})
	recvBounded(t, ctx, rev.entered, "the review start")

	// Wait starts while the review is held. The count it sees on return must
	// include the review's line, which is logged before the review's done
	// channel closes.
	started, idle := poolIdle(pool)
	recvBounded(t, ctx, started, "Wait starting")
	close(rev.release)
	recvBounded(t, ctx, idle, "Wait returning")
	if n := len(logs.get()); n != 1 {
		t.Fatalf("Wait returned with %d lines logged, want 1: it returned before the review ended", n)
	}
	if got := logs.get(); len(got) != 1 || !strings.Contains(got[0], "gt-bad") || !strings.Contains(got[0], "om unreachable") {
		t.Errorf("logged %q, want one line naming the bead and the error", got)
	}
}

func TestReviewPoolLogsNothingForCancelledReview(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, map[string]error{"gt-stop": context.Canceled})
	logs := &logRecorder{}
	pool := &ReviewPool{Reviewer: rev, Max: 1, Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-stop", Head: "4444444"}})
	recvBounded(t, ctx, rev.entered, "the review start")
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if got := logs.get(); len(got) != 0 {
		t.Errorf("logged %q for a cancelled review, want nothing", got)
	}
}

func TestReviewPoolSameBeadRunsOneReview(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	// One slot: the later offer starts only because the first review has ended.
	pool := &ReviewPool{Reviewer: rev, Max: 1}
	first := land.Work{BeadID: "gt-once", Head: "5555555"}

	pool.Offer(context.Background(), []land.Work{first})
	recvBounded(t, ctx, rev.entered, "the first review start")

	// A second offer for the same bead while the first is held starts nothing,
	// and Wait does not return until the first review ends.
	pool.Offer(context.Background(), []land.Work{{BeadID: first.BeadID, Head: "6666666"}})
	started, idle := poolIdle(pool)
	recvBounded(t, ctx, started, "Wait starting")
	select {
	case <-idle:
		t.Fatal("Wait returned while the first review was still running")
	default:
	}
	close(rev.release)
	recvBounded(t, ctx, idle, "Wait returning")
	if n := len(rev.reviewed()); n != 1 {
		t.Fatalf("reviewer called %d times for one bead offered twice, want 1", n)
	}

	// Once the first has finished its entry is gone: an offer starts a new review.
	pool.Offer(context.Background(), []land.Work{{BeadID: first.BeadID, Head: "7777777"}})
	recvBounded(t, ctx, rev.entered, "the new review start")
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")
	if n := len(rev.reviewed()); n != 2 {
		t.Errorf("reviewer called %d times after a later offer, want 2", n)
	}
}

// TestReviewPoolNotStartedTwice checks that a bead already under review is not
// started again however often the list is offered: the pool has room for more
// works than the list holds, so only the running check keeps a later offer from
// starting a second review of a bead.
func TestReviewPoolNotStartedTwice(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	works := []land.Work{
		{BeadID: "gt-alpha", Head: "1111111"},
		{BeadID: "gt-beta", Head: "2222222"},
		{BeadID: "gt-gamma", Head: "3333333"},
	}
	rev := newFakeAheadReviewer(t, ctx, nil)
	// Room for every work: the cap refuses nothing, so the running check is the
	// only thing that can keep an offer from starting a bead twice. Every
	// duplicate a defective pool might start must fit in rev.entered too, so the
	// failure names the count instead of a review stuck on a full channel.
	rev.entered = make(chan land.Work, len(works)*3)
	pool := &ReviewPool{Reviewer: rev, Max: len(works) + 1}

	pool.Offer(context.Background(), works)
	for n := 0; n < len(works); n++ {
		recvBounded(t, ctx, rev.entered, "a review start")
	}

	// Every review is held, so every bead is still running when the list is
	// offered twice more.
	pool.Offer(context.Background(), works)
	pool.Offer(context.Background(), works)

	// Let every review the pool started finish. Each start reaches the reviewer,
	// so the count of calls is what tells a second review of a bead apart.
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if got, want := reviewCounts(rev), map[string]int{"gt-alpha": 1, "gt-beta": 1, "gt-gamma": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("reviews per bead %v after offering the list three times, want each bead once: %v", got, want)
	}
}

// TestReviewPoolFree checks how many more reviews may start: Max less the
// entries running, never below 0. It fills the running set directly: a pool
// holds one entry per review under way, and the past-the-cap row tests the
// clamp itself, not how the pool came to hold that many.
func TestReviewPoolFree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running int
		want    int
	}{
		{name: "none running", running: 0, want: 2},
		{name: "one running", running: 1, want: 1},
		{name: "at the cap", running: 2, want: 0},
		{name: "past the cap", running: 3, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := &ReviewPool{Max: 2, running: map[string]*aheadRun{}}
			for n := 0; n < tt.running; n++ {
				pool.running[fmt.Sprintf("gt-running-%d", n)] = &aheadRun{head: "0000000"}
			}

			pool.mu.Lock()
			got := pool.free()
			pool.mu.Unlock()

			if got != tt.want {
				t.Errorf("free() with %d entries running and Max %d = %d, want %d", tt.running, pool.Max, got, tt.want)
			}
		})
	}
}

// TestReviewPoolCapsAtMax checks that Offer starts at most Max reviews: with
// Max 2 and four works, the first two start and the last two are neither
// started nor queued, so the reviewer is asked only for the first two.
func TestReviewPoolCapsAtMax(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	pool := &ReviewPool{Reviewer: rev, Max: 2}

	pool.Offer(context.Background(), []land.Work{
		{BeadID: "gt-one", Head: "1111111"},
		{BeadID: "gt-two", Head: "2222222"},
		{BeadID: "gt-three", Head: "3333333"},
		{BeadID: "gt-four", Head: "4444444"},
	})
	for n := 0; n < 2; n++ {
		recvBounded(t, ctx, rev.entered, "a review start")
	}
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	got := rev.reviewed()
	if len(got) != 2 {
		t.Fatalf("reviewer called %d times with Max 2 and four works, want 2", len(got))
	}
	// The works that fit are the first two of the list; their reviews enter
	// the reviewer in no promised order.
	gotBeads := map[string]bool{}
	for _, w := range got {
		gotBeads[w.BeadID] = true
	}
	if want := map[string]bool{"gt-one": true, "gt-two": true}; !reflect.DeepEqual(gotBeads, want) {
		t.Errorf("reviewed beads %v, want the first two of the list %v", gotBeads, want)
	}
}

// TestReviewPoolZeroMaxIsOne checks that a pool built without a Max runs one
// review at a time: Max 0 and Max -1 both count as 1, so of three offered works
// exactly one starts.
func TestReviewPoolZeroMaxIsOne(t *testing.T) {
	t.Parallel()
	for _, max := range []int{0, -1} {
		t.Run(fmt.Sprintf("Max %d", max), func(t *testing.T) {
			t.Parallel()
			ctx := waitCtx(t)
			rev := newFakeAheadReviewer(t, ctx, nil)
			pool := &ReviewPool{Reviewer: rev, Max: max}

			pool.Offer(context.Background(), []land.Work{
				{BeadID: "gt-first", Head: "1111111"},
				{BeadID: "gt-second", Head: "2222222"},
				{BeadID: "gt-third", Head: "3333333"},
			})
			recvBounded(t, ctx, rev.entered, "the one review start")
			close(rev.release)
			recvBounded(t, ctx, idleOf(pool), "the pool going idle")

			got := rev.reviewed()
			if len(got) != 1 {
				t.Fatalf("reviewer called %d times for three works with Max %d, want 1", len(got), max)
			}
			if got[0].BeadID != "gt-first" {
				t.Errorf("reviewed %s with Max %d, want the first work gt-first", got[0].BeadID, max)
			}
		})
	}
}

// TestReviewPoolLandingOrder pins that the pool keeps no queue: with Max 2 and
// four works the first two start and the rest are dropped, so the third starts
// only at the later Offer that follows a review's end, and the fourth waits for
// the second slot.
func TestReviewPoolLandingOrder(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	works := []land.Work{
		{BeadID: "gt-first", Head: "1111111"},
		{BeadID: "gt-second", Head: "2222222"},
		{BeadID: "gt-third", Head: "3333333"},
		{BeadID: "gt-fourth", Head: "4444444"},
	}
	// One release channel per work, so the test ends one review and leaves the
	// rest running.
	release := map[string]chan struct{}{}
	for _, w := range works {
		release[w.BeadID] = make(chan struct{})
	}
	rev := newFakeAheadReviewer(t, ctx, nil)
	rev.perWork = release
	pool := &ReviewPool{Reviewer: rev, Max: 2}

	// At the cap the first two works start; the third and fourth are not
	// queued.
	pool.Offer(context.Background(), works)
	for n := 0; n < 2; n++ {
		recvBounded(t, ctx, rev.entered, "a review start")
	}
	if got, want := reviewCounts(rev), map[string]int{"gt-first": 1, "gt-second": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews per bead %v after the first offer, want the first two works only: %v", got, want)
	}

	// The first review ends and frees its slot. Nothing starts on its own.
	firstDone := reviewEnded(t, pool, "gt-first")
	close(release["gt-first"])
	recvBounded(t, ctx, firstDone, "the first review ending")
	if got, want := reviewCounts(rev), map[string]int{"gt-first": 1, "gt-second": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews per bead %v after the first review ended, want nothing new: %v", got, want)
	}

	// The list is offered again: the third takes the freed slot and the fourth
	// still does not fit.
	pool.Offer(context.Background(), works[1:])
	if w := recvBounded(t, ctx, rev.entered, "the third review start"); w.BeadID != "gt-third" {
		t.Fatalf("started %s after the list was offered again, want the third work gt-third", w.BeadID)
	}
	if got, want := reviewCounts(rev), map[string]int{"gt-first": 1, "gt-second": 1, "gt-third": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews per bead %v after the list was offered again, want the third and not the fourth: %v", got, want)
	}

	// The second review ends; at the next offer the fourth takes that slot.
	secondDone := reviewEnded(t, pool, "gt-second")
	close(release["gt-second"])
	recvBounded(t, ctx, secondDone, "the second review ending")
	pool.Offer(context.Background(), works[2:])
	if w := recvBounded(t, ctx, rev.entered, "the fourth review start"); w.BeadID != "gt-fourth" {
		t.Fatalf("started %s after the second slot freed, want the fourth work gt-fourth", w.BeadID)
	}
	if got, want := reviewCounts(rev), map[string]int{"gt-first": 1, "gt-second": 1, "gt-third": 1, "gt-fourth": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews per bead %v after the second slot freed, want each work once: %v", got, want)
	}

	close(release["gt-third"])
	close(release["gt-fourth"])
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")
}

// reviewEnded returns the channel the pool closes when bead's review has ended
// and left it. Wait covers every review at once, and the pool has no per-review
// event, so a test that must let exactly one review finish waits on its done:
// the entry exists by the time its start has been received, and it leaves
// before the channel closes.
func reviewEnded(t *testing.T, pool *ReviewPool, bead string) <-chan struct{} {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	run, ok := pool.running[bead]
	if !ok {
		t.Fatalf("no review running for %s, so none can be waited for", bead)
	}
	return run.done
}

// reviewCounts returns how many reviews the reviewer was asked for, per bead:
// the pool launches its reviews in the order given, but Go promises no order
// for their entry into the reviewer.
func reviewCounts(f *fakeAheadReviewer) map[string]int {
	counts := map[string]int{}
	for _, w := range f.reviewed() {
		counts[w.BeadID]++
	}
	return counts
}

func TestReviewPoolWithoutReviewerStartsNothing(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	logs := &logRecorder{}
	pool := &ReviewPool{Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-none", Head: "6666666"}})
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if n := len(logs.get()); n != 1 {
		t.Errorf("logged %d lines with no reviewer, want 1", n)
	}
}
