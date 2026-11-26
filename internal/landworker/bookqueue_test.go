package landworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
)

// hold waits for release, or returns at the test's bound.
func hold(ctx context.Context, release <-chan struct{}) {
	select {
	case <-release:
	case <-ctx.Done():
	}
}

// TestBookQueueRunsInOrder: jobs are serial, Add returns while one runs, a panic is survived.
func TestBookQueueRunsInOrder(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	var q BookQueue

	var order []string
	firstEntered, firstRelease := make(chan struct{}), make(chan struct{})
	secondEntered, afterPanic := make(chan struct{}), make(chan struct{})

	q.Add("gt-first", func() {
		order = append(order, "first start")
		close(firstEntered)
		hold(ctx, firstRelease)
		order = append(order, "first return")
	})
	recvBounded(t, ctx, firstEntered, "the first job starting")

	// Add on its own goroutine: one that waits for the blocked job fails by name.
	added := make(chan struct{})
	go func() {
		q.Add("gt-second", func() {
			order = append(order, "second start")
			close(secondEntered)
		})
		close(added)
	}()
	recvBounded(t, ctx, added, "Add returning while the first job is blocked")
	select {
	case <-secondEntered:
		t.Fatal("the second job started before the first returned")
	default:
	}

	close(firstRelease)
	recvBounded(t, ctx, secondEntered, "the second job starting after the first returned")
	q.Add("gt-panic", func() { panic("bookkeeping blew up") })
	q.Add("gt-last", func() { close(afterPanic) })
	recvBounded(t, ctx, afterPanic, "the job after the panicking one starting")

	if want := []string{"first start", "first return", "second start"}; !reflect.DeepEqual(order, want) {
		t.Errorf("jobs ran in order %v, want %v", order, want)
	}
}

// TestBookQueueHolds: Holds spans a bead's job, from Add until that job returns.
func TestBookQueueHolds(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	var q BookQueue

	firstEntered, firstRelease := make(chan struct{}), make(chan struct{})
	secondEntered, secondRelease := make(chan struct{}), make(chan struct{})
	thirdEntered := make(chan struct{})

	q.Add("gt-first", func() { close(firstEntered); hold(ctx, firstRelease) })
	recvBounded(t, ctx, firstEntered, "the first job starting")
	q.Add("gt-second", func() { close(secondEntered); hold(ctx, secondRelease) })
	if !q.Holds("gt-first") || !q.Holds("gt-second") {
		t.Errorf("Holds with the first running and the second queued = %v, %v, want true, true",
			q.Holds("gt-first"), q.Holds("gt-second"))
	}

	close(firstRelease)
	recvBounded(t, ctx, secondEntered, "the second job starting after the first returned")
	if q.Holds("gt-first") {
		t.Error("Holds(gt-first) = true after its job returned, want false")
	}
	if !q.Holds("gt-second") {
		t.Error("Holds(gt-second) = false while its job runs, want true")
	}

	// A third job starting proves the second returned: its hold is released first.
	q.Add("gt-third", func() { close(thirdEntered) })
	close(secondRelease)
	recvBounded(t, ctx, thirdEntered, "the third job starting after the second returned")
	if q.Holds("gt-second") {
		t.Error("Holds(gt-second) = true after its job returned, want false")
	}
}

// TestBookQueueLaterRunsInRunLater: Later stores fn without running it, and
// RunLater runs each stored fn once, in the order the jobs stored them.
func TestBookQueueLaterRunsInRunLater(t *testing.T) {
	t.Parallel()

	var q BookQueue
	var order []string
	q.Later(func() { order = append(order, "first") })
	q.Later(func() { order = append(order, "second") })
	if len(order) != 0 {
		t.Fatalf("Later ran %v; want nothing until RunLater", order)
	}

	q.RunLater()
	if want := []string{"first", "second"}; !reflect.DeepEqual(order, want) {
		t.Errorf("RunLater ran %v; want %v", order, want)
	}
	q.RunLater()
	if want := []string{"first", "second"}; !reflect.DeepEqual(order, want) {
		t.Errorf("a second RunLater ran %v; want each function once, %v", order, want)
	}
}

// laterListBeads checks the work stored with Later ran before the pass listed.
type laterListBeads struct {
	Beads
	ran    *bool
	listed *bool
	t      *testing.T
}

func (b *laterListBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	*b.listed = true
	if !*b.ran {
		b.t.Error("the pass listed the ready board before running the work stored with Later")
	}
	return b.Beads.List(opts)
}

// TestBookQueueLaterRunsBeforeList: a pass with a queue runs the work a job
// stored with Later before it lists the ready board.
func TestBookQueueLaterRunsBeforeList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var q BookQueue
	var ran, listed bool
	q.Later(func() { ran = true })
	h.w.Books = &q
	h.w.Beads = &laterListBeads{Beads: h.w.Beads, ran: &ran, listed: &listed, t: t}

	h.w.Pass(context.Background())
	if !listed {
		t.Fatal("the pass did not list the ready board")
	}
	if !ran {
		t.Fatal("the pass listed without running the work stored with Later")
	}
}

// TestBookQueueBusyIdle: Begin runs once for two jobs queued together, before
// the first starts, and End once after the second returns; nil hooks are fine.
func TestBookQueueBusyIdle(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	var q BookQueue

	// events records each step in causal order: a buffered send never blocks.
	events := make(chan string, 8)
	q.Begin = func() { events <- "begin" }
	q.End = func() { events <- "end" }

	first, firstRelease := make(chan struct{}), make(chan struct{})
	second, secondRelease := make(chan struct{}), make(chan struct{})
	q.Add("gt-first", func() { events <- "first start"; close(first); hold(ctx, firstRelease); events <- "first return" })
	recvBounded(t, ctx, first, "the first job starting")

	// The second is added while the first runs, so both fall under one Begin.
	q.Add("gt-second", func() { events <- "second start"; close(second); hold(ctx, secondRelease); events <- "second return" })
	close(firstRelease)
	recvBounded(t, ctx, second, "the second job starting after the first returned")
	close(secondRelease)

	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, recvBounded(t, ctx, events, "the next recorded step"))
	}
	if want := []string{"begin", "first start", "first return", "second start", "second return", "end"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the run recorded %v, want %v", got, want)
	}

	// A queue whose hooks are nil runs its jobs too.
	var plain BookQueue
	ran := make(chan struct{})
	plain.Add("gt-plain", func() { close(ran) })
	recvBounded(t, ctx, ran, "a job on a queue with nil hooks")
}

// TestBookQueueWait: Wait returns nil once the queue is empty and the
// context's error while a job is still queued. It runs in a synctest bubble so
// a Wait that returns on the first job to finish is caught, not raced with.
func TestBookQueueWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := waitCtx(t)
		// settle is the bubble's barrier: it returns once every goroutine in the
		// bubble is blocked, so the check that Wait has not returned reads a
		// settled state instead of racing a goroutine that has not run yet.
		settle := synctest.Wait
		var q BookQueue
		wait := q.Wait
		first, firstRelease := make(chan struct{}), make(chan struct{})
		second, secondRelease := make(chan struct{}), make(chan struct{})
		// Release the last job however the test ends, so a failure does not
		// leave the drain goroutine blocked in the bubble.
		var release sync.Once
		defer release.Do(func() { close(secondRelease) })
		q.Add("gt-first", func() { close(first); hold(ctx, firstRelease) })
		recvBounded(t, ctx, first, "the first job starting")
		q.Add("gt-second", func() { close(second); hold(ctx, secondRelease) })

		waitUntil, cancel := context.WithCancel(ctx)
		defer cancel()
		waited := make(chan error, 1)
		go func() { waited <- wait(waitUntil) }()

		// The waiter is blocked in Wait before the first job returns.
		settle()
		close(firstRelease)
		recvBounded(t, ctx, second, "the second job starting after the first returned")
		settle()
		select {
		case err := <-waited:
			t.Fatalf("Wait returned %v with the second job still running, want it to keep waiting", err)
		default:
		}

		// The last job is still blocked, so Wait reports the context.
		cancel()
		settle()
		if err := recvBounded(t, ctx, waited, "Wait returning on cancel"); !errors.Is(err, context.Canceled) {
			t.Errorf("Wait with a job still blocked = %v, want context.Canceled", err)
		}

		// With the last job released, an empty queue returns nil at once.
		release.Do(func() { close(secondRelease) })
		settle()
		if err := wait(ctx); err != nil {
			t.Errorf("Wait after the last job = %v, want nil", err)
		}
	})
}

// TestBookQueueAddDuringEnd: a job added while End runs opens the next busy
// period from the drain goroutine, so the previous End returns before the next
// Begin runs, the new job waits for it, and Wait does not return while End runs.
func TestBookQueueAddDuringEnd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := waitCtx(t)
		// settle is the bubble's barrier: it returns once every goroutine in the
		// bubble is blocked, so the "has not run" and "has not returned" checks
		// below read a settled state instead of racing a goroutine not yet run.
		settle := synctest.Wait

		var q BookQueue
		events := make(chan string, 8)
		q.Begin = func() { events <- "begin" }

		var endCalls int // read and written by the drain goroutine alone
		firstEnd := make(chan struct{})
		endRelease := make(chan struct{})
		var release sync.Once
		// Release the blocked End however the test ends, so every goroutine in
		// the bubble can finish.
		defer release.Do(func() { close(endRelease) })
		q.End = func() {
			events <- "end"
			endCalls++
			if endCalls == 1 {
				close(firstEnd)
				hold(ctx, endRelease)
			}
		}

		q.Add("gt-first", func() { events <- "first job" })
		recvBounded(t, ctx, firstEnd, "the first period reaching End")

		// The queue is still active while End runs, so this Add queues without
		// calling Begin.
		secondRan := make(chan struct{})
		q.Add("gt-second", func() { events <- "second job"; close(secondRan) })

		// waited carries Wait's result; Wait returns on ctx, and the select's
		// ctx clause lets this goroutine end with it too.
		waited := make(chan error, 1)
		go func() {
			select {
			case waited <- q.Wait(ctx):
			case <-ctx.Done():
			}
		}()

		settle()
		select {
		case <-secondRan:
			t.Fatal("the job added during End ran before End returned")
		default:
		}
		select {
		case err := <-waited:
			t.Fatalf("Wait returned %v while End still ran, want it to keep waiting", err)
		default:
		}

		release.Do(func() { close(endRelease) })
		settle()

		var got []string
		for i := 0; i < 6; i++ {
			got = append(got, recvBounded(t, ctx, events, "the next recorded step"))
		}
		if want := []string{"begin", "first job", "end", "begin", "second job", "end"}; !reflect.DeepEqual(got, want) {
			t.Errorf("the run recorded %v, want %v", got, want)
		}
		if err := recvBounded(t, ctx, waited, "Wait returning after the last End"); err != nil {
			t.Errorf("Wait after the queue emptied = %v, want nil", err)
		}
	})
}

// TestBookQueuePanicIsLogged: a panicking job logs one entry whose first line
// names its bead and the panic value and that carries the stack, and the job
// after it runs.
func TestBookQueuePanicIsLogged(t *testing.T) {
	t.Parallel()
	ctx := waitCtx(t)
	var q BookQueue

	logged := make(chan string, 4)
	q.Logf = func(format string, args ...any) {
		logged <- fmt.Sprintf(format, args...)
	}
	q.Add("gt-panic", func() { panic("bookkeeping blew up") })
	next := make(chan struct{})
	q.Add("gt-next", func() { close(next) })
	recvBounded(t, ctx, next, "the job after the panicking one starting")

	entry := recvBounded(t, ctx, logged, "the log entry for the panicking job")
	first, rest, _ := strings.Cut(entry, "\n")
	if !strings.Contains(first, "gt-panic") || !strings.Contains(first, "bookkeeping blew up") {
		t.Errorf("the log entry starts %q, want its first line to name gt-panic and the panic value", first)
	}
	if !strings.Contains(rest, "goroutine ") {
		t.Errorf("the log entry after its first line is %q, want the stack", rest)
	}
	select {
	case second := <-logged:
		t.Errorf("the panicking job logged twice; the second entry is %q", second)
	default:
	}
}

// waitDrained runs wait until it returns, failing the test when ctx's bound
// passes first, so a Wait that never returns fails by name.
func waitDrained(t *testing.T, ctx context.Context, wait func(context.Context) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- wait(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatalf("Wait did not return within the test's bound: %v", ctx.Err())
		return ctx.Err()
	}
}

// TestBookQueueOverdue: a job still running past bookJobBound is reported once
// with its bead, and a job that ends within the bound is not reported at all
// (gt-2hqjbi).
func TestBookQueueOverdue(t *testing.T) {
	t.Parallel()
	epoch := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	t.Run("a job held past the bound is reported once with its bead", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		clk := clockwork.NewFakeClockAt(epoch)
		var q BookQueue
		q.Clock = clk
		logged := make(chan string, 4)
		q.Logf = func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) }

		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
		// Release the job however the test ends, so the queue's drain finishes.
		t.Cleanup(releaseJob)
		q.Add("gt-hung", func() { close(started); hold(ctx, release) })
		recvBounded(t, ctx, started, "the job starting")

		// The drain waits on the clock for the bound, so advancing before it
		// does reports nothing.
		if err := clk.BlockUntilContext(ctx, 1); err != nil {
			t.Fatalf("the queue never waited on the clock for the job's bound: %v", err)
		}
		clk.Advance(bookJobBound)

		entry := recvBounded(t, ctx, logged, "the report for the job held past the bound")
		if !strings.Contains(entry, "gt-hung") {
			t.Errorf("the report %q does not name the bead gt-hung", entry)
		}

		releaseJob()
		if err := waitDrained(t, ctx, q.Wait); err != nil {
			t.Fatalf("Wait after the job ended = %v, want nil", err)
		}
		select {
		case second := <-logged:
			t.Errorf("the job was reported a second time: %q", second)
		default:
		}
	})

	t.Run("a job that ends within the bound reports nothing", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		clk := clockwork.NewFakeClockAt(epoch)
		var q BookQueue
		q.Clock = clk
		logged := make(chan string, 1)
		q.Logf = func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) }

		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
		// Release the job however the test ends, so the queue's drain finishes.
		t.Cleanup(releaseJob)
		q.Add("gt-quick", func() { close(started); hold(ctx, release) })
		recvBounded(t, ctx, started, "the job starting")

		// Move the clock to just short of the bound while the job is still
		// running: the drain waits on the clock for the bound, and a job that
		// ends before the timer fires is never reported.
		if err := clk.BlockUntilContext(ctx, 1); err != nil {
			t.Fatalf("the queue never waited on the clock for the job's bound: %v", err)
		}
		clk.Advance(bookJobBound - 1)

		releaseJob()
		if err := waitDrained(t, ctx, q.Wait); err != nil {
			t.Fatalf("Wait after the job ended = %v, want nil", err)
		}
		select {
		case got := <-logged:
			t.Errorf("a job that ended within the bound was reported: %q", got)
		default:
		}
	})
}

// lockedBuffer keeps a log.Logger's output readable while the drain goroutine
// writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestBookQueuePanicWithNilLogfGoesToTheStandardLogger: with no Logf set, a
// panicking job is still logged, through the standard log package, with its
// bead and the panic value, and the job after it runs.
//
//testpolicy:allow parallel — captures the process-wide standard logger, which another test must not write mid-run
func TestBookQueuePanicWithNilLogfGoesToTheStandardLogger(t *testing.T) {
	ctx := waitCtx(t)
	var q BookQueue

	var logged lockedBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	q.Add("gt-nil-panic", func() { panic("no Logf set") })
	next := make(chan struct{})
	q.Add("gt-next", func() { close(next) })
	recvBounded(t, ctx, next, "the job after the panicking one starting")

	if got := logged.String(); !strings.Contains(got, "gt-nil-panic") || !strings.Contains(got, "no Logf set") {
		t.Errorf("the standard logger got %q, want a line naming gt-nil-panic and the panic value", got)
	}
}
