package landworker

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
)

// bookJobBound is how long one job may run before the queue reports it as
// still running. A job writes a bead's record and reaps its branch, so one
// past this is hung; naming its bead lets an operator see what a hold or a
// maintenance pass is waiting on instead of only that something is (gt-2hqjbi).
const bookJobBound = 10 * time.Minute

// BookQueue runs bookkeeping jobs one at a time, in the order they were added,
// on a goroutine of its own; the zero value is ready to use. A job that runs
// past bookJobBound is reported once, naming its bead, through Overdue and
// Logf.
type BookQueue struct {
	// Begin runs when the queue goes from empty to busy, before the job that
	// opened the period starts; End runs when that period's last job returns.
	// End returns before the next period's Begin runs. Either may be nil; set
	// both before the queue is used.
	Begin func()
	End   func()

	// Logf logs a job that panicked, with the bead it was for, the panic
	// value and the stack, and a job still running past bookJobBound. It may be
	// called from more than one goroutine. nil logs through the standard log
	// package; set it before the queue is used.
	Logf func(string, ...any)

	// Clock is the time source the queue waits a job's bound on. nil uses the
	// wall clock; set it before the queue is used.
	Clock clockwork.Clock

	// Overdue is called once, on the drain goroutine, when a job has run past
	// bookJobBound and is still running: with the bead it was for and the
	// bound. The same moment is logged through Logf. nil calls nothing; set it
	// before the queue is used.
	Overdue func(beadID string, bound time.Duration)

	mu     sync.Mutex
	jobs   []bookJob
	held   map[string]int // bead -> jobs added and not yet returned
	active bool           // a busy period is open: jobs queued, a job or hook running
	later  []func()       // stored by Later, run by the next RunLater
	idle   chan struct{}  // closed when active goes false; non-nil while active
}

type bookJob struct {
	beadID string
	job    func()
}

// Add queues job as bookkeeping for beadID and returns without waiting for a
// running job; Holds reports beadID from here until job returns.
func (q *BookQueue) Add(beadID string, job func()) {
	q.mu.Lock()
	if q.held == nil {
		q.held = map[string]int{}
	}
	q.held[beadID]++
	q.jobs = append(q.jobs, bookJob{beadID: beadID, job: job})
	busy := !q.active
	if busy {
		q.active = true
		q.idle = make(chan struct{})
	}
	q.mu.Unlock()

	if !busy {
		return
	}
	// This Add opens the period, so it runs Begin; every other Add sees active
	// and leaves Begin to the drain. Running it before the drain starts keeps
	// it before this period's End, which the drain alone runs.
	if q.Begin != nil {
		q.Begin()
	}
	go q.drain()
}

// Holds reports whether beadID's bookkeeping is queued or running: true from
// its Add until its job returns, false after.
func (q *BookQueue) Holds(beadID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held[beadID] > 0
}

// Later stores fn to run at the next RunLater, in the order the jobs stored
// them: a job uses it for work that touches Worker or RedMain state, which only
// the worker's own goroutine may run (gt-rx2kl7).
func (q *BookQueue) Later(fn func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.later = append(q.later, fn)
}

// RunLater runs, in the order they were stored and once each, the functions
// Later stored, on the calling goroutine. The lock never covers a function, so
// one that adds to the queue does not deadlock; work it stores waits for the
// next RunLater.
func (q *BookQueue) RunLater() {
	q.mu.Lock()
	later := q.later
	q.later = nil
	q.mu.Unlock()
	for _, fn := range later {
		fn()
	}
}

// clock is the time source the queue waits a job's bound on: Clock when it is
// set, the wall clock otherwise.
func (q *BookQueue) clock() clockwork.Clock {
	if q.Clock != nil {
		return q.Clock
	}
	return clockwork.NewRealClock()
}

// logf logs a panicking job: through Logf when it is set, and through the
// standard log package otherwise, so a failed job is never silent.
func (q *BookQueue) logf(format string, args ...any) {
	if q.Logf != nil {
		q.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// drain runs the jobs one at a time, opening each busy period with Begin and
// closing it with End, and exits once no job is left and End has returned. The
// lock never covers a job or a hook, so Add, Holds and Wait answer while one
// runs.
//
// active stays true while End runs, so an Add that lands then queues its job
// without calling Begin. The loop sees that job with the period already ended
// and runs Begin itself, so End returns before the next Begin and Wait, which
// waits on idle, cannot return before End. Clearing active under the lock Add
// takes loses no Add racing the last job, and closes the idle channel Wait is on.
func (q *BookQueue) drain() {
	ended := false
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			if !ended {
				q.mu.Unlock()
				if q.End != nil {
					q.End()
				}
				ended = true
				continue
			}
			q.active = false
			close(q.idle)
			q.idle = nil
			q.mu.Unlock()
			return
		}
		if ended {
			// A job arrived while End ran; its Add saw active and called no
			// Begin, so this period opens here.
			q.mu.Unlock()
			if q.Begin != nil {
				q.Begin()
			}
			ended = false
			continue
		}
		next := q.jobs[0]
		q.jobs[0] = bookJob{}
		q.jobs = q.jobs[1:]
		q.mu.Unlock()

		// The job runs on its own goroutine so the drain can report it once it
		// outlives the bound; the drain still runs one job at a time, and moves
		// on only after this one returns.
		done := make(chan struct{})
		go func() {
			defer close(done)
			// A panicking job must not stop the queue: recover it, log it with the
			// bead it was for and the stack, and run the next job.
			defer func() {
				if r := recover(); r != nil {
					q.logf("bookqueue: job for %s panicked: %v\n%s", next.beadID, r, debug.Stack())
				}
			}()
			next.job()
		}()

		bound := q.clock().NewTimer(bookJobBound)
		select {
		case <-done:
			bound.Stop()
		case <-bound.Chan():
			// The job is still running: tell the queue's owner and name its bead
			// once, then keep waiting for it, so the queue's order is unchanged
			// and the report is never repeated for the same job.
			if q.Overdue != nil {
				q.Overdue(next.beadID, bookJobBound)
			}
			q.logf("bookqueue: the job for %s has run longer than %s and is still running", next.beadID, bookJobBound)
			<-done
		}

		q.mu.Lock()
		if n := q.held[next.beadID]; n > 1 {
			q.held[next.beadID] = n - 1
		} else {
			delete(q.held, next.beadID)
		}
		q.mu.Unlock()
	}
}

// Wait returns nil once the queue holds no job and the last period's End has
// returned, and ctx's error when ctx ends first. An empty queue returns at once.
func (q *BookQueue) Wait(ctx context.Context) error {
	q.mu.Lock()
	if !q.active {
		q.mu.Unlock()
		return nil
	}
	idle := q.idle
	q.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
