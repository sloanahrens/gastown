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
// bound passes first.
func recvBounded[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-waitCtx(t).Done():
		t.Fatalf("%s did not happen within the test's bound", what)
		var zero T
		return zero
	}
}

// waitPool calls pool.Wait on the goroutine it runs on. The caller's bound is
// the receive from the channel the goroutine closes after it, never the call
// itself; the select only skips a Wait the test has already given up on.
func waitPool(bound <-chan struct{}, pool *ReviewPool) {
	select {
	case <-bound:
	default:
		pool.Wait()
	}
}

// poolIdle runs pool.Wait on its own goroutine and returns a channel that
// closes when Wait returns, so a test bounds the wait with recvBounded.
func poolIdle(t *testing.T, pool *ReviewPool) <-chan struct{} {
	bound := waitCtx(t).Done()
	idle := make(chan struct{})
	go func() {
		waitPool(bound, pool)
		close(idle)
	}()
	return idle
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

func newFakeAheadReviewer(t *testing.T, fail map[string]error) *fakeAheadReviewer {
	t.Helper()
	return &fakeAheadReviewer{entered: make(chan land.Work, 8), release: make(chan struct{}), bound: waitCtx(t).Done(), fail: fail}
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

func TestReviewPoolStartsInOrder(t *testing.T) {
	t.Parallel()
	rev := newFakeAheadReviewer(t, nil)
	pool := &ReviewPool{Reviewer: rev}

	pool.Offer(context.Background(), []land.Work{
		{BeadID: "gt-first", Head: "1111111"},
		{BeadID: "gt-second", Head: "2222222"},
	})
	started := map[string]string{}
	for n := 0; n < 2; n++ {
		w := recvBounded(t, rev.entered, "a review start")
		started[w.BeadID] = w.Head
	}
	close(rev.release)
	recvBounded(t, poolIdle(t, pool), "the pool going idle")

	if want := map[string]string{"gt-first": "1111111", "gt-second": "2222222"}; !reflect.DeepEqual(started, want) {
		t.Errorf("reviews started %v, want one per work %v", started, want)
	}
	if n := len(rev.reviewed()); n != 2 {
		t.Errorf("reviewer called %d times, want 2", n)
	}
}

func TestReviewPoolWaitReturnsWhenIdle(t *testing.T) {
	t.Parallel()
	rev := newFakeAheadReviewer(t, map[string]error{"gt-bad": errors.New("om unreachable")})
	logs := &logRecorder{}
	pool := &ReviewPool{Reviewer: rev, Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-bad", Head: "3333333"}})
	recvBounded(t, rev.entered, "the review start")

	// Wait starts while the review is held. The count it sees on return must
	// include the review's line, which is logged before the review's done
	// channel closes.
	started := make(chan struct{})
	idle := make(chan struct{})
	bound := waitCtx(t).Done()
	go func() {
		close(started)
		waitPool(bound, pool)
		close(idle)
	}()
	recvBounded(t, started, "Wait starting")
	close(rev.release)
	recvBounded(t, idle, "Wait returning")
	if n := len(logs.get()); n != 1 {
		t.Fatalf("Wait returned with %d lines logged, want 1: it returned before the review ended", n)
	}
	if got := logs.get(); len(got) != 1 || !strings.Contains(got[0], "gt-bad") || !strings.Contains(got[0], "om unreachable") {
		t.Errorf("logged %q, want one line naming the bead and the error", got)
	}
}

func TestReviewPoolLogsNothingForCancelledReview(t *testing.T) {
	t.Parallel()
	rev := newFakeAheadReviewer(t, map[string]error{"gt-stop": context.Canceled})
	logs := &logRecorder{}
	pool := &ReviewPool{Reviewer: rev, Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-stop", Head: "4444444"}})
	recvBounded(t, rev.entered, "the review start")
	close(rev.release)
	recvBounded(t, poolIdle(t, pool), "the pool going idle")

	if got := logs.get(); len(got) != 0 {
		t.Errorf("logged %q for a cancelled review, want nothing", got)
	}
}

func TestReviewPoolDoesNotStartBeadTwice(t *testing.T) {
	t.Parallel()
	rev := newFakeAheadReviewer(t, nil)
	pool := &ReviewPool{Reviewer: rev}
	work := land.Work{BeadID: "gt-once", Head: "5555555"}

	pool.Offer(context.Background(), []land.Work{work})
	recvBounded(t, rev.entered, "the review start")
	pool.mu.Lock()
	first := pool.running[work.BeadID]
	pool.mu.Unlock()

	// Checked at once, while the first review is held, so the result does not
	// depend on how the goroutines are scheduled.
	pool.Offer(context.Background(), []land.Work{work})
	pool.mu.Lock()
	if pool.running[work.BeadID] != first {
		t.Errorf("second offer replaced the review already running for %s", work.BeadID)
	}
	pool.mu.Unlock()

	close(rev.release)
	recvBounded(t, poolIdle(t, pool), "the pool going idle")
	if n := len(rev.reviewed()); n != 1 {
		t.Errorf("reviewer called %d times for one bead, want 1", n)
	}
}

func TestReviewPoolWithoutReviewerStartsNothing(t *testing.T) {
	t.Parallel()
	logs := &logRecorder{}
	pool := &ReviewPool{Logf: logs.logf}

	pool.Offer(context.Background(), []land.Work{{BeadID: "gt-none", Head: "6666666"}})
	recvBounded(t, poolIdle(t, pool), "the pool going idle")

	if n := len(logs.get()); n != 1 {
		t.Errorf("logged %d lines with no reviewer, want 1", n)
	}
}
