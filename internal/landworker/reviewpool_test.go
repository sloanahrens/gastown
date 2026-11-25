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
// the test closes release or its bound passes. A review whose bead is in fail
// returns that error.
type fakeAheadReviewer struct {
	entered chan land.Work
	release chan struct{}
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
	select {
	case <-f.release:
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
	pool := &ReviewPool{Reviewer: rev}

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
	pool := &ReviewPool{Reviewer: rev, Logf: logs.logf}

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
	pool := &ReviewPool{Reviewer: rev, Logf: logs.logf}

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
	pool := &ReviewPool{Reviewer: rev}
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
