package landworker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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
// one, or its context is cancelled, or its bound passes. A review whose bead is
// in fail returns that error; one whose bead is in panics panics with that
// value instead of returning. When stopped is set it receives each work whose
// context ended, and when heldAfterCancel is set the review then stays in
// ReviewAhead until the test closes it, so a test can act on a cancelled review
// that has not yet left the pool.
type fakeAheadReviewer struct {
	entered         chan land.Work
	release         chan struct{}
	perWork         map[string]chan struct{}
	bound           <-chan struct{}
	fail            map[string]error
	panics          map[string]string
	stopped         chan land.Work
	heldAfterCancel chan struct{}

	mu       sync.Mutex
	calls    []land.Work
	returned bool
}

func newFakeAheadReviewer(t *testing.T, ctx context.Context, fail map[string]error) *fakeAheadReviewer {
	t.Helper()
	return &fakeAheadReviewer{entered: make(chan land.Work, 8), release: make(chan struct{}), bound: ctx.Done(), fail: fail}
}

func (f *fakeAheadReviewer) ReviewAhead(ctx context.Context, w land.Work) (land.AheadOutcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, w)
	f.mu.Unlock()
	f.entered <- w
	if msg, ok := f.panics[w.BeadID]; ok {
		panic(msg)
	}
	release := f.release
	if ch, ok := f.perWork[w.BeadID]; ok {
		release = ch
	}
	select {
	case <-release:
	case <-ctx.Done():
		if f.stopped != nil {
			f.stopped <- w
			if f.heldAfterCancel != nil {
				select {
				case <-f.heldAfterCancel:
				case <-f.bound:
					return f.returnFrom("", errors.New("review held past the test's bound"))
				}
			}
		}
		return f.returnFrom("", ctx.Err())
	case <-f.bound:
		return f.returnFrom("", errors.New("review held past the test's bound"))
	}
	if err := f.fail[w.BeadID]; err != nil {
		return f.returnFrom("", err)
	}
	return f.returnFrom(land.AheadSkipped, nil)
}

// returnFrom records that the review is returning and yields its result. It
// runs after the review's work, so a test can tell an End called before the
// reviewer returned from one called after.
func (f *fakeAheadReviewer) returnFrom(out land.AheadOutcome, err error) (land.AheadOutcome, error) {
	f.mu.Lock()
	f.returned = true
	f.mu.Unlock()
	return out, err
}

// hasReturned reports whether any review has returned from ReviewAhead.
func (f *fakeAheadReviewer) hasReturned() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.returned
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

// holdFake is a pool's Begin/End pair for a landing hold: begin answers with
// reply, and the counters show what Offer asked of it.
type holdFake struct {
	mu     sync.Mutex
	reply  bool
	begins int
	ends   int
}

func (h *holdFake) begin() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.begins++
	return h.reply
}

func (h *holdFake) end() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends++
}

func (h *holdFake) counts() (begins, ends int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.begins, h.ends
}

// set changes the hold's answer, so a test can lift a hold between offers while
// the pool keeps the same Begin hook it was built with.
func (h *holdFake) set(reply bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reply = reply
}

// fakeAheadHints answers Hint from a fixed table: a bead in heads answers that
// head, a bead in fail answers its error, and every other bead has no stored
// review. It records every bead it is asked about.
type fakeAheadHints struct {
	heads map[string]string
	fail  map[string]error

	mu    sync.Mutex
	asked []string
}

func (f *fakeAheadHints) Hint(bead string) (string, bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, bead)
	f.mu.Unlock()
	if err := f.fail[bead]; err != nil {
		return "", false, err
	}
	return f.heads[bead], false, nil
}

// askedBeads returns the beads Hint was asked about.
func (f *fakeAheadHints) askedBeads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
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

// TestReviewPoolAwaitReturnsWhenDone pins that Await waits while the bead's
// review runs and returns once that review has ended. It runs in a synctest
// bubble: the bubble's barrier shows Await still waiting after the review
// started, so a wait that returns at once is caught, not raced with.
func TestReviewPoolAwaitReturnsWhenDone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			bead = "gt-await"
			head = "1111111"
		)
		ctx := waitCtx(t)
		rev := newFakeAheadReviewer(t, ctx, nil)
		pool := &ReviewPool{Reviewer: rev, Max: 1}

		pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: head}})
		recvBounded(t, ctx, rev.entered, "the review start")

		awaited := make(chan struct{})
		go func() {
			pool.Await(context.Background(), bead, head)
			close(awaited)
		}()

		// The review is held, so the waiter is blocked in Await when the
		// bubble settles.
		synctest.Wait()
		select {
		case <-awaited:
			t.Fatalf("Await returned with the bead's review still running, want it to wait")
		default:
		}

		close(rev.release)
		recvBounded(t, ctx, awaited, "Await returning after the review ended")
		synctest.Wait()
		if headAt, running := runningHeadAt(t, pool, bead); running {
			t.Errorf("Await returned with the bead's review still in the pool at %s, want the review gone", headAt)
		}
	})
}

// TestReviewPoolAwaitIgnoresOtherHead pins that Await waits for a review of the
// caller's head only: with a review of another head of the bead running, the
// wait returns at once, because that verdict cannot judge the head the caller
// holds (gt-rudbr6). It runs in a synctest bubble, whose barrier settles both
// waiters before either is checked.
func TestReviewPoolAwaitIgnoresOtherHead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			bead      = "gt-await"
			heldHead  = "1111111"
			otherHead = "2222222"
		)
		ctx := waitCtx(t)
		rev := newFakeAheadReviewer(t, ctx, nil)
		pool := &ReviewPool{Reviewer: rev, Max: 1}

		pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: heldHead}})
		recvBounded(t, ctx, rev.entered, "the review start")

		otherReturned := make(chan struct{})
		go func() {
			pool.Await(context.Background(), bead, otherHead)
			close(otherReturned)
		}()
		heldReturned := make(chan struct{})
		go func() {
			pool.Await(context.Background(), bead, heldHead)
			close(heldReturned)
		}()
		synctest.Wait()

		select {
		case <-otherReturned:
		default:
			t.Errorf("Await for head %s did not return while only head %s is reviewed, want it to return at once", otherHead, heldHead)
		}
		select {
		case <-heldReturned:
			t.Fatalf("Await for the reviewed head %s returned while its review still runs, want it to wait", heldHead)
		default:
		}
		if headAt, running := runningHeadAt(t, pool, bead); !running || headAt != heldHead {
			t.Errorf("the pool holds the bead's review at %s (running %v) after the other-head Await returned, want head %s still reviewed", headAt, running, heldHead)
		}

		close(rev.release)
		recvBounded(t, ctx, heldReturned, "Await returning after the review of its head ended")
	})
}

// TestReviewPoolAwaitNothingRunning pins that Await returns at once for a bead
// with no review running: the rows cover an idle pool and a pool busy with
// another bead's review, so a wait on the pool rather than on the bead's own
// entry is caught.
func TestReviewPoolAwaitNothingRunning(t *testing.T) {
	t.Parallel()
	const (
		bead = "gt-await"
		head = "1111111"
	)
	tests := []struct {
		name string
		// other is a bead whose review runs while Await is called; empty
		// leaves the pool idle.
		other string
	}{
		{name: "idle pool"},
		{name: "another bead's review running", other: "gt-other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx := waitCtx(t)
				rev := newFakeAheadReviewer(t, ctx, nil)
				pool := &ReviewPool{Reviewer: rev, Max: 2}
				if tt.other != "" {
					pool.Offer(context.Background(), []land.Work{{BeadID: tt.other, Head: head}})
					recvBounded(t, ctx, rev.entered, "the other bead's review start")
				}

				awaited := make(chan struct{})
				go func() {
					pool.Await(context.Background(), bead, head)
					close(awaited)
				}()
				synctest.Wait()
				select {
				case <-awaited:
				default:
					t.Fatalf("Await did not return at once for a bead with no review running")
				}

				if tt.other != "" {
					close(rev.release)
					recvBounded(t, ctx, idleOf(pool), "the other bead's review ending")
				}
			})
		})
	}
}

// TestReviewPoolAwaitReturnsOnCancel pins that a cancelled ctx ends the wait
// while the bead's review still runs: Await returns, the pool still holds that
// review, and the review returns only at its own release.
func TestReviewPoolAwaitReturnsOnCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			bead = "gt-await"
			head = "1111111"
		)
		ctx := waitCtx(t)
		rev := newFakeAheadReviewer(t, ctx, nil)
		pool := &ReviewPool{Reviewer: rev, Max: 1}

		pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: head}})
		recvBounded(t, ctx, rev.entered, "the review start")

		waitUntil, cancel := context.WithCancel(ctx)
		awaited := make(chan struct{})
		go func() {
			pool.Await(waitUntil, bead, head)
			close(awaited)
		}()

		synctest.Wait()
		select {
		case <-awaited:
			t.Fatalf("Await returned before its context was cancelled, want it to wait")
		default:
		}

		cancel()
		synctest.Wait()
		select {
		case <-awaited:
		default:
			t.Fatalf("Await did not return after its context was cancelled")
		}
		if headAt, running := runningHeadAt(t, pool, bead); !running {
			t.Errorf("the bead's review left the pool with the cancelled wait, want it still running")
		} else if headAt != head {
			t.Errorf("the pool holds the bead's review at %s, want %s still running", headAt, head)
		}
		if rev.hasReturned() {
			t.Errorf("the review returned with the cancelled wait, want it to keep running until its release")
		}

		close(rev.release)
		recvBounded(t, ctx, idleOf(pool), "the review ending at the test's release")
		if !rev.hasReturned() {
			t.Errorf("the reviewer never returned after its release")
		}
	})
}

// TestReviewPoolErrorNotRetried pins that the head whose review failed is not
// offered again, while a new head of the same bead still is: the pool keeps the
// head each bead's last review ended at, so a broken om call is not repeated on
// every pass, and a seat that pushed a new commit is reviewed (gt-xa01yc).
func TestReviewPoolErrorNotRetried(t *testing.T) {
	t.Parallel()
	const (
		bead    = "gt-error"
		oldHead = "1111111"
		newHead = "2222222"
	)
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, map[string]error{bead: errors.New("om unreachable")})
	pool := &ReviewPool{Reviewer: rev, Max: 1}

	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	recvBounded(t, ctx, rev.entered, "the failing review start")
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle after the failure")
	if got := reviewCounts(rev); got[bead] != 1 {
		t.Fatalf("reviews of %s %d after the failing review, want 1", bead, got[bead])
	}

	// The same head is a head the pool tried: the offer starts no review.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	recvBounded(t, ctx, idleOf(pool), "the pool going idle after the repeat offer")
	if head, ok := runningHeadAt(t, pool, bead); ok {
		t.Errorf("the repeat offer started a review of %s at %s, want none: the pool tried that head", bead, head)
	}
	if n := len(rev.reviewed()); n != 1 {
		t.Fatalf("reviewer called %d times after the failed head was offered again, want 1", n)
	}

	// A head the pool has not tried is reviewed.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	w := recvBounded(t, ctx, rev.entered, "the new head's review start")
	if w.BeadID != bead || w.Head != newHead {
		t.Errorf("started a review of %s at %s, want %s at %s", w.BeadID, w.Head, bead, newHead)
	}
	recvBounded(t, ctx, idleOf(pool), "the pool going idle after the new head")
	if n := len(rev.reviewed()); n != 2 {
		t.Fatalf("reviewer called %d times, want the failed head then the new head", n)
	}
}

// TestReviewPoolCancelledDoesNotBlockNewHead pins that a review cancelled for a
// new head leaves the new head reviewable. The cancelled review records only
// its own head as tried, so the next offer starts the new head; an offer of the
// old head while the new review runs is a head the pool tried, so it starts
// nothing and cancels nothing (gt-bnc6ah). A pool that read the running entry
// before the tried memo would cancel the new head's review here.
func TestReviewPoolCancelledDoesNotBlockNewHead(t *testing.T) {
	t.Parallel()
	const (
		bead    = "gt-seat"
		oldHead = "1111111"
		newHead = "2222222"
	)
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	// The old review is held by the test, so only the cancel for the new head
	// ends it.
	rev.perWork = map[string]chan struct{}{bead: make(chan struct{})}
	logs := &logRecorder{}
	pool := &ReviewPool{Reviewer: rev, Max: 1, Logf: logs.logf}

	// The old head's review starts, and the offer of the new head cancels it.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	recvBounded(t, ctx, rev.entered, "the old head's review start")
	oldDone := reviewEnded(t, pool, bead)
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	recvBounded(t, ctx, oldDone, "the cancelled old review leaving the pool")
	if got := logs.get(); len(got) != 1 {
		t.Fatalf("logged %q for the replaced head, want one cancel line", got)
	}

	// The cancelled review does not block the head that replaced it: the next
	// offer starts the new head's review.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	w := recvBounded(t, ctx, rev.entered, "the new head's review start")
	if w.BeadID != bead || w.Head != newHead {
		t.Fatalf("started a review of %s at %s, want %s at %s", w.BeadID, w.Head, bead, newHead)
	}

	// The old head is a head the pool has tried: offering it again while the new
	// head is under review starts nothing, cancels nothing and logs nothing.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	if head, ok := runningHeadAt(t, pool, bead); !ok || head != newHead {
		t.Errorf("after the tried old head was offered again the pool holds the bead's review at head %q (running %v), want the new head %s still in place", head, ok, newHead)
	}
	if got := logs.get(); len(got) != 1 {
		t.Errorf("offering the tried old head changed the log to %q, want the one cancel line: it must not cancel the new head's review", got)
	}
	if n := len(rev.reviewed()); n != 2 {
		t.Fatalf("reviewer called %d times before the new review ended, want the old head then the new head", n)
	}

	// Let the new head's review end; it is the only one left.
	close(rev.perWork[bead])
	recvBounded(t, ctx, idleOf(pool), "the pool going idle after the new head")
	got := rev.reviewed()
	if len(got) != 2 {
		t.Fatalf("reviewer called %d times, want the old head then the new head: %v", len(got), got)
	}
	if got[0] != (land.Work{BeadID: bead, Head: oldHead}) {
		t.Errorf("first review was %+v, want the old head %s", got[0], oldHead)
	}
	if got[1] != (land.Work{BeadID: bead, Head: newHead}) {
		t.Errorf("second review was %+v, want the new head %s", got[1], newHead)
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

// TestReviewPoolSupersededHeadIsCancelled pins that a bead offered at a head
// its seat has replaced stops that review: the reviewer returns on its context,
// one line names the bead and both heads, and the offer starts nothing in its
// place. The rows cover a pool with room and one at Max, where the cancel must
// come before the cap, a repeat offer of the running head, which must leave the
// review alone, and a repeat offer of the new head while the cancelled review is
// still leaving, which must not cancel or log a second time.
func TestReviewPoolSupersededHeadIsCancelled(t *testing.T) {
	t.Parallel()
	const (
		bead    = "gt-seat"
		oldHead = "1111111"
		newHead = "2222222"
	)
	tests := []struct {
		name     string
		max      int
		offers   []string
		wantStop bool
		// holdCancelled keeps the cancelled review in the pool between the
		// offers, so the later offer meets a run that is already cancelled.
		holdCancelled bool
	}{
		{name: "room in the pool", max: 2, offers: []string{newHead}, wantStop: true},
		{name: "at Max", max: 1, offers: []string{newHead}, wantStop: true},
		{name: "same head again", max: 1, offers: []string{oldHead}},
		{name: "new head offered twice", max: 1, offers: []string{newHead, newHead}, wantStop: true, holdCancelled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := waitCtx(t)
			rev := newFakeAheadReviewer(t, ctx, nil)
			// The old review is held by the test, so only a cancel ends it.
			rev.perWork = map[string]chan struct{}{bead: make(chan struct{})}
			if tt.holdCancelled {
				rev.stopped = make(chan land.Work, 1)
				rev.heldAfterCancel = make(chan struct{})
			}
			logs := &logRecorder{}
			pool := &ReviewPool{Reviewer: rev, Max: tt.max, Logf: logs.logf}

			pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
			recvBounded(t, ctx, rev.entered, "the old head's review start")
			oldDone := reviewEnded(t, pool, bead)

			for i, head := range tt.offers {
				pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: head}})
				if i == 0 && tt.holdCancelled {
					recvBounded(t, ctx, rev.stopped, "the cancelled review seeing its context end")
				}
			}

			if !tt.wantStop {
				if got := logs.get(); len(got) != 0 {
					t.Fatalf("offering the same head again logged %q, want nothing", got)
				}
				close(rev.perWork[bead])
				recvBounded(t, ctx, oldDone, "the review ending at the test's release")
				if n := len(rev.reviewed()); n != 1 {
					t.Errorf("reviewer called %d times, want the one review of the old head", n)
				}
				return
			}

			if tt.holdCancelled {
				close(rev.heldAfterCancel)
			}
			recvBounded(t, ctx, oldDone, "the old review ending when its context is cancelled")
			recvBounded(t, ctx, idleOf(pool), "the pool going idle")
			if n := len(rev.reviewed()); n != 1 {
				t.Errorf("reviewer called %d times after the new head was offered, want only the old head: the new head starts at the next offer", n)
			}
			got := logs.get()
			if len(got) != 1 {
				t.Fatalf("logged %q, want one line for the cancelled review", got)
			}
			for _, want := range []string{bead, oldHead, newHead} {
				if !strings.Contains(got[0], want) {
					t.Errorf("logged %q, want it to name %q", got[0], want)
				}
			}
		})
	}
}

// TestReviewPoolSupersededHeadStartsTheNewOne pins that the head which replaced a
// running review is reviewed once that review has gone: with Max 2 the cap has
// room, so the running entry alone keeps the new head back. Offering the new head
// cancels the old review and starts nothing while it is still in the pool; the
// offer after it has returned starts the new head's review with the new work.
func TestReviewPoolSupersededHeadStartsTheNewOne(t *testing.T) {
	t.Parallel()
	const (
		bead    = "gt-seat"
		oldHead = "1111111"
		newHead = "2222222"
	)
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	// The old review is held by the test, so only a cancel ends it, and it then
	// stays in the pool until the test lets it go.
	rev.perWork = map[string]chan struct{}{bead: make(chan struct{})}
	rev.stopped = make(chan land.Work, 1)
	rev.heldAfterCancel = make(chan struct{})
	// Two slots: with one review running the cap has room, so only the running
	// check keeps the new head from starting.
	pool := &ReviewPool{Reviewer: rev, Max: 2}

	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	recvBounded(t, ctx, rev.entered, "the old head's review start")
	oldDone := reviewEnded(t, pool, bead)

	// The new head cancels the old review; it is held after its cancel, so its
	// entry is still in the pool when the new head is offered again. Neither
	// offer may start a review of the new head while that entry remains.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	recvBounded(t, ctx, rev.stopped, "the cancelled review seeing its context end")
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})

	// The cancelled review is still in the pool, and the pool still holds it at
	// the old head: the new head has not been started in its place.
	if head, ok := runningHeadAt(t, pool, bead); !ok || head != oldHead {
		t.Fatalf("after the new head was offered twice, the pool holds the bead's review at head %q (running %v), want the old head %s still in place: the new head starts only once that review has gone", head, ok, oldHead)
	}

	// Let the cancelled review leave. The old review's channel is closed too, so
	// the new head's later review ends as soon as it starts.
	close(rev.perWork[bead])
	close(rev.heldAfterCancel)
	recvBounded(t, ctx, oldDone, "the old review ending")
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if got := rev.reviewed(); len(got) != 1 {
		t.Fatalf("reviewer called %d times while the old review was still in the pool, want only the old head: %v", len(got), got)
	}

	// The old review has returned and left the pool: this offer starts the new
	// head's review with the new work.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	w := recvBounded(t, ctx, rev.entered, "the new head's review start")
	if w.BeadID != bead || w.Head != newHead {
		t.Errorf("started a review of %s at %s, want %s at %s", w.BeadID, w.Head, bead, newHead)
	}
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	got := rev.reviewed()
	if len(got) != 2 {
		t.Fatalf("reviewer called %d times, want the old head then the new head: %v", len(got), got)
	}
	if got[0] != (land.Work{BeadID: bead, Head: oldHead}) {
		t.Errorf("first review was %+v, want the old head %s", got[0], oldHead)
	}
	if got[1] != (land.Work{BeadID: bead, Head: newHead}) {
		t.Errorf("second review was %+v, want the new head %s", got[1], newHead)
	}
}

// TestReviewPoolSupersededRespectsMax pins that replacing a head never lifts the
// count over Max: with Max 1 the old review is held in the pool after its cancel,
// so an offer of the new head starts nothing while that entry remains, and the
// new head's review starts only from an offer after the old review has returned.
// A pool that freed the slot at the cancel would run the two reviews at once.
func TestReviewPoolSupersededRespectsMax(t *testing.T) {
	t.Parallel()
	const (
		bead    = "gt-seat"
		oldHead = "1111111"
		newHead = "2222222"
	)
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	// The old review ends only on its cancel, and then stays in ReviewAhead until
	// the test lets it go, so its pool entry is still there for the later offers.
	rev.stopped = make(chan land.Work, 1)
	rev.heldAfterCancel = make(chan struct{})
	pool := &ReviewPool{Reviewer: rev, Max: 1}

	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	recvBounded(t, ctx, rev.entered, "the old head's review start")
	oldDone := reviewEnded(t, pool, bead)

	// The new head cancels the old review; the cancel is held, so the old entry
	// is still in the pool.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	recvBounded(t, ctx, rev.stopped, "the cancelled review seeing its context end")

	// At Max 1 that entry alone keeps the new head back: a pool that gave the
	// slot up at the cancel would start a second review beside the running one.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	if head, ok := runningHeadAt(t, pool, bead); !ok || head != oldHead {
		t.Fatalf("with Max 1 the pool holds the bead's review at head %q (running %v) while the old review is still in ReviewAhead, want the old head %s", head, ok, oldHead)
	}
	if n := len(rev.reviewed()); n != 1 {
		t.Fatalf("reviewer called %d times while the old head's review was still running, want 1: two reviews ran at once with Max 1", n)
	}
	select {
	case w := <-rev.entered:
		t.Fatalf("a review of %s at %s started while the old head's review was still running, want nothing: two reviews run at once with Max 1", w.BeadID, w.Head)
	default:
	}

	// The old review returns and leaves the pool. Only an offer after that starts
	// the new head, and that review runs alone.
	close(rev.heldAfterCancel)
	recvBounded(t, ctx, oldDone, "the old review ending")
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	w := recvBounded(t, ctx, rev.entered, "the new head's review start")
	if w.BeadID != bead || w.Head != newHead {
		t.Errorf("started a review of %s at %s, want %s at %s", w.BeadID, w.Head, bead, newHead)
	}
	if head, ok := runningHeadAt(t, pool, bead); !ok || head != newHead {
		t.Errorf("the pool holds the bead's review at head %q (running %v), want the new head %s", head, ok, newHead)
	}
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	got := rev.reviewed()
	if len(got) != 2 {
		t.Fatalf("reviewer called %d times, want the old head then the new head: %v", len(got), got)
	}
	if got[0] != (land.Work{BeadID: bead, Head: oldHead}) {
		t.Errorf("first review was %+v, want the old head %s", got[0], oldHead)
	}
	if got[1] != (land.Work{BeadID: bead, Head: newHead}) {
		t.Errorf("second review was %+v, want the new head %s", got[1], newHead)
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

	// A second offer for the same bead at the same head while the first is held
	// starts nothing, and Wait does not return until the first review ends.
	pool.Offer(context.Background(), []land.Work{{BeadID: first.BeadID, Head: first.Head}})
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

// runningHeadAt returns the head the pool has registered as running for bead,
// and whether it has one. start registers the entry under p.mu before it
// launches the review, so the entry is settled the moment Offer returns. A
// review under way may not yet have reached the reviewer, so the reviewer's own
// record cannot tell "never started" from "not scheduled yet".
func runningHeadAt(t *testing.T, pool *ReviewPool, bead string) (string, bool) {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	run, ok := pool.running[bead]
	if !ok {
		return "", false
	}
	return run.head, true
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

// TestReviewPoolHoldStartsNothing pins that a landing hold stops ahead reviews:
// while Begin answers false, an offer starts no review, asks Begin only for the
// first review it would have started, and calls no End for a review it did not
// start. The rows offer one work and several, so a pool that started any of them
// fails by name.
func TestReviewPoolHoldStartsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		max   int
		works []land.Work
	}{
		{name: "one work", max: 1, works: []land.Work{
			{BeadID: "gt-held", Head: "1111111"},
		}},
		{name: "several works", max: 3, works: []land.Work{
			{BeadID: "gt-held", Head: "1111111"},
			{BeadID: "gt-also", Head: "2222222"},
			{BeadID: "gt-third", Head: "3333333"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := waitCtx(t)
			rev := newFakeAheadReviewer(t, ctx, nil)
			hold := &holdFake{reply: false}
			pool := &ReviewPool{Reviewer: rev, Max: tt.max, Begin: hold.begin, End: hold.end}

			pool.Offer(context.Background(), tt.works)

			// The pool records a review under p.mu before it launches the
			// goroutine, so an entry here is a review started, not one merely
			// not yet scheduled.
			for _, w := range tt.works {
				if head, ok := runningHeadAt(t, pool, w.BeadID); ok {
					t.Errorf("under a hold the pool started a review of %s at %s, want none", w.BeadID, head)
				}
			}
			if got := rev.reviewed(); len(got) != 0 {
				t.Errorf("reviewer called %d times under a hold, want 0: %v", len(got), got)
			}
			if begins, ends := hold.counts(); begins != 1 || ends != 0 {
				t.Errorf("Begin called %d times and End %d under a hold, want Begin 1 and End 0: the offer starts nothing more and ends no review it did not start", begins, ends)
			}
			recvBounded(t, ctx, idleOf(pool), "the pool going idle under a hold")
		})
	}
}

// TestReviewPoolNilBeginStarts pins that a pool with no Begin starts its
// reviews: an unset Begin is not a landing hold, so Offer starts the reviews the
// cap allows. Two works and a slot for each also catch a pool that read the
// missing hook as a refusal: the refusal stops the rest of the list as well, so
// asserting one reviewer call would not be enough.
func TestReviewPoolNilBeginStarts(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	works := []land.Work{
		{BeadID: "gt-open", Head: "1111111"},
		{BeadID: "gt-also", Head: "2222222"},
	}
	// Begin is left unset: the pool has no hold to ask.
	pool := &ReviewPool{Reviewer: rev, Max: len(works)}

	pool.Offer(context.Background(), works)
	for n := 0; n < len(works); n++ {
		recvBounded(t, ctx, rev.entered, "a review start")
	}
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if got, want := reviewCounts(rev), map[string]int{"gt-open": 1, "gt-also": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("reviews per bead %v with Begin unset, want every work started: %v", got, want)
	}
}

// TestReviewPoolHoldLiftedStartsAgain pins that a lifted hold lets the next
// offer start reviews again: while Begin answers false the offer starts nothing,
// and the offer of the same works after Begin turns true starts one review per
// work. Offer asks Begin afresh on each call, so a pool that latched the first
// false and never asked again would start nothing here (gt-vb9qod).
func TestReviewPoolHoldLiftedStartsAgain(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	hold := &holdFake{reply: false}
	pool := &ReviewPool{Reviewer: rev, Max: 2, Begin: hold.begin, End: hold.end}
	works := []land.Work{
		{BeadID: "gt-held", Head: "1111111"},
		{BeadID: "gt-also", Head: "2222222"},
	}

	// The hold is on: the offer starts nothing and asks Begin once, for the first
	// review it would have started.
	pool.Offer(context.Background(), works)
	for _, w := range works {
		if head, ok := runningHeadAt(t, pool, w.BeadID); ok {
			t.Errorf("under a hold the pool started a review of %s at %s, want none", w.BeadID, head)
		}
	}
	if got := rev.reviewed(); len(got) != 0 {
		t.Errorf("reviewer called %d times under a hold, want 0: %v", len(got), got)
	}
	if begins, ends := hold.counts(); begins != 1 || ends != 0 {
		t.Errorf("Begin called %d times and End %d under a hold, want Begin 1 and End 0", begins, ends)
	}

	// The hold lifts: the next offer of the same works starts both reviews.
	hold.set(true)
	pool.Offer(context.Background(), works)
	started := map[string]string{}
	for n := 0; n < len(works); n++ {
		w := recvBounded(t, ctx, rev.entered, "a review start after the hold lifted")
		started[w.BeadID] = w.Head
	}
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle after the reviews ended")

	if want := map[string]string{"gt-held": "1111111", "gt-also": "2222222"}; !reflect.DeepEqual(started, want) {
		t.Errorf("reviews started %v after the hold lifted, want one per work %v", started, want)
	}
	if begins, ends := hold.counts(); begins < 2 || ends != len(works) {
		t.Errorf("Begin called %d times and End %d after the hold lifted, want Begin asked again and End %d", begins, ends, len(works))
	}
}

// TestReviewPoolHoldStillCancelsReplaced pins that a landing hold does not stop
// the cancel of a stale review: with Begin answering false, an offer of a bead
// whose seat has moved to a new head cancels the running review of the old head,
// starts nothing and asks no Begin. A pool that returned on the hold before the
// running check would leave the stale review running for the whole hold
// (gt-sor91y).
func TestReviewPoolHoldStillCancelsReplaced(t *testing.T) {
	t.Parallel()
	const (
		bead    = "gt-held-seat"
		oldHead = "1111111"
		newHead = "2222222"
	)
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	// The old review is held by the test, so only a cancel ends it.
	rev.perWork = map[string]chan struct{}{bead: make(chan struct{})}
	rev.stopped = make(chan land.Work, 1)
	logs := &logRecorder{}
	hold := &holdFake{reply: true}
	pool := &ReviewPool{Reviewer: rev, Max: 2, Begin: hold.begin, End: hold.end, Logf: logs.logf}

	// The old head's review starts under a lifted hold.
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: oldHead}})
	recvBounded(t, ctx, rev.entered, "the old head's review start")
	oldDone := reviewEnded(t, pool, bead)

	// The hold is on. The offer of the new head still cancels the old review; the
	// cancel is decided under p.mu before Offer returns, so its line is there at
	// once and no review of the new head was started in its place.
	hold.set(false)
	pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: newHead}})
	got := logs.get()
	if len(got) != 1 {
		t.Fatalf("logged %q offering the new head under a hold, want one cancel line", got)
	}
	for _, want := range []string{bead, oldHead, newHead} {
		if !strings.Contains(got[0], want) {
			t.Errorf("logged %q, want it to name %q", got[0], want)
		}
	}
	if n := len(rev.reviewed()); n != 1 {
		t.Errorf("reviewer called %d times, want only the old head: the new head starts at a later offer", n)
	}

	// The cancelled review sees its context end, leaves the pool and ends once.
	recvBounded(t, ctx, rev.stopped, "the cancelled review seeing its context end")
	recvBounded(t, ctx, oldDone, "the cancelled old review leaving the pool")
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")
	if begins, ends := hold.counts(); begins != 1 || ends != 1 {
		t.Errorf("Begin called %d times and End %d, want 1 and 1: only the old review began, and it ended", begins, ends)
	}
}

// TestReviewPoolEndAlwaysCalled pins that every started review calls End
// exactly once, after the reviewer returned, on each way a review can end: a
// normal return, an error and a cancelled context. The reviewer records its own
// return, so an End called before the reviewer left fails by name; a nil End is
// the no-op every other pool test here leaves unset (gt-oi5mmf).
func TestReviewPoolEndAlwaysCalled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fail   error
		cancel bool
	}{
		{name: "normal return"},
		{name: "error", fail: errors.New("om unreachable")},
		{name: "cancelled context", cancel: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := waitCtx(t)
			rev := newFakeAheadReviewer(t, ctx, nil)
			if tt.fail != nil {
				rev.fail = map[string]error{"gt-ended": tt.fail}
			}
			var mu sync.Mutex
			var endedAfterReturn []bool
			pool := &ReviewPool{Reviewer: rev, Max: 1, End: func() {
				mu.Lock()
				defer mu.Unlock()
				endedAfterReturn = append(endedAfterReturn, rev.hasReturned())
			}}

			offerCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			pool.Offer(offerCtx, []land.Work{{BeadID: "gt-ended", Head: "1111111"}})
			recvBounded(t, ctx, rev.entered, "the review start")
			if tt.cancel {
				cancel()
			} else {
				close(rev.release)
			}
			// Wait returning means the review left the pool, which the pool does
			// only after End, so the count below is settled.
			recvBounded(t, ctx, idleOf(pool), "the pool going idle")

			mu.Lock()
			got := append([]bool(nil), endedAfterReturn...)
			mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("End called %d times for one review, want exactly once", len(got))
			}
			if !got[0] {
				t.Errorf("End ran before the reviewer returned, want it after")
			}
		})
	}
}

// TestReviewPoolPanicIsRecovered pins that a panicking review is contained: the
// pool logs one line naming the bead and the panic, calls End once as for any
// review that began, drops the entry so Wait returns, and still starts a later
// offer. Without the recover the panic in the review goroutine ends the test
// process, so no pool that does not contain it can pass (gt-4m0ccu).
func TestReviewPoolPanicIsRecovered(t *testing.T) {
	t.Parallel()
	const boom = "reviewer exploded"
	ctx := waitCtx(t)
	rev := newFakeAheadReviewer(t, ctx, nil)
	rev.panics = map[string]string{"gt-boom": boom}
	logs := &logRecorder{}
	hold := &holdFake{reply: true}
	pool := &ReviewPool{Reviewer: rev, Max: 1, Begin: hold.begin, End: hold.end, Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-boom", Head: "1111111"}})
	recvBounded(t, ctx, rev.entered, "the panicking review start")
	recvBounded(t, ctx, idleOf(pool), "the pool going idle after the panic")

	if begins, ends := hold.counts(); begins != 1 || ends != 1 {
		t.Fatalf("Begin called %d times and End %d for one panicking review, want 1 and 1", begins, ends)
	}
	got := logs.get()
	if len(got) != 1 {
		t.Fatalf("logged %q for one panicking review, want one line", got)
	}
	for _, want := range []string{"gt-boom", boom} {
		if !strings.Contains(got[0], want) {
			t.Errorf("logged %q, want it to name %q", got[0], want)
		}
	}
	if head, ok := runningHeadAt(t, pool, "gt-boom"); ok {
		t.Errorf("after the panic the pool still holds a review of gt-boom at %s", head)
	}

	// The slot the panicking review held is free: a later offer starts a review.
	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-after", Head: "2222222"}})
	w := recvBounded(t, ctx, rev.entered, "the review after the panic")
	if w.BeadID != "gt-after" || w.Head != "2222222" {
		t.Errorf("started a review of %s at %s, want gt-after at 2222222", w.BeadID, w.Head)
	}
	close(rev.release)
	recvBounded(t, ctx, idleOf(pool), "the pool going idle")

	if n := len(rev.reviewed()); n != 2 {
		t.Errorf("reviewer called %d times, want the panicking review then the later one", n)
	}
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

// TestReviewPoolHintSkips checks that Offer reuses a stored review: a hint at
// the offered head starts nothing, a hint at another head leaves that work to be
// reviewed, a hint error starts nothing and is logged once, and no hints at all
// review the work. The error row offers the list twice: the head it recorded as
// tried is what keeps the second offer from asking and logging again.
func TestReviewPoolHintSkips(t *testing.T) {
	t.Parallel()
	const (
		bead = "gt-stored"
		head = "1111111"
		old  = "2222222"
	)
	tests := []struct {
		name       string
		hints      *fakeAheadHints
		offers     int
		wantReview bool
		wantAsked  int
		wantLogs   int
	}{
		{
			name:      "a hint at the same head is skipped",
			hints:     &fakeAheadHints{heads: map[string]string{bead: head}},
			offers:    1,
			wantAsked: 1,
		},
		{
			name:       "a hint at another head is reviewed",
			hints:      &fakeAheadHints{heads: map[string]string{bead: old}},
			offers:     1,
			wantReview: true,
			wantAsked:  1,
		},
		{
			name:      "a hint error is skipped and logged once",
			hints:     &fakeAheadHints{fail: map[string]error{bead: errors.New("unreadable record")}},
			offers:    2,
			wantAsked: 1,
			wantLogs:  1,
		},
		{
			name:       "no hints reviews",
			offers:     1,
			wantReview: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := waitCtx(t)
			rev := newFakeAheadReviewer(t, ctx, nil)
			logs := &logRecorder{}
			pool := &ReviewPool{Reviewer: rev, Max: 1, Logf: logs.logf}
			if tt.hints != nil {
				pool.Hints = tt.hints
			}

			for n := 0; n < tt.offers; n++ {
				pool.Offer(context.Background(), []land.Work{{BeadID: bead, Head: head}})
			}
			if tt.wantReview {
				if w := recvBounded(t, ctx, rev.entered, "the review to start"); w.BeadID != bead || w.Head != head {
					t.Errorf("started a review of %s at %s, want %s at %s", w.BeadID, w.Head, bead, head)
				}
				close(rev.release)
			}
			recvBounded(t, ctx, idleOf(pool), "the pool going idle")

			gotReviews := len(rev.reviewed())
			if tt.wantReview && gotReviews != 1 {
				t.Errorf("reviewer called %d times, want the work reviewed once", gotReviews)
			}
			if !tt.wantReview && gotReviews != 0 {
				t.Errorf("reviewer called %d times, want the stored review reused", gotReviews)
			}
			asked := 0
			if tt.hints != nil {
				asked = len(tt.hints.askedBeads())
			}
			if asked != tt.wantAsked {
				t.Errorf("hint asked %d times, want %d", asked, tt.wantAsked)
			}
			if lines := logs.get(); len(lines) != tt.wantLogs {
				t.Errorf("logged %d lines, want %d: %v", len(lines), tt.wantLogs, lines)
			} else if tt.wantLogs > 0 && !strings.Contains(lines[0], bead) {
				t.Errorf("log line %q does not name %s", lines[0], bead)
			}
		})
	}
}
