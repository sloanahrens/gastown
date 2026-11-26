package landworker

import (
	"context"
	"sync"
)

// BookQueue runs bookkeeping jobs one at a time, in the order they were added,
// on a goroutine of its own; the zero value is ready to use.
type BookQueue struct {
	// Begin runs as the queue goes from empty to busy, in Add before its first
	// job starts; End runs as it empties again, on the drain goroutine after its
	// last job returns. Either may be nil; set both before the queue is used.
	Begin func()
	End   func()

	mu     sync.Mutex
	jobs   []bookJob
	held   map[string]int // bead -> jobs added and not yet returned
	active bool           // the drain goroutine is running
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
	// Begin runs before the drain starts, so it cannot come after the End of
	// the period it opens.
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

// drain runs the jobs one at a time and exits once none is left, running End
// as it goes. The lock never covers a job or a hook, so Add, Holds and Wait
// answer while one runs; clearing active under the same lock Add takes loses no
// Add racing the last job, and closes the idle channel Wait is on.
func (q *BookQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.active = false
			close(q.idle)
			q.idle = nil
			q.mu.Unlock()
			break
		}
		next := q.jobs[0]
		q.jobs[0] = bookJob{}
		q.jobs = q.jobs[1:]
		q.mu.Unlock()

		// A panicking job must not stop the queue: recover it, run the next.
		func() {
			defer func() { _ = recover() }()
			next.job()
		}()

		q.mu.Lock()
		if n := q.held[next.beadID]; n > 1 {
			q.held[next.beadID] = n - 1
		} else {
			delete(q.held, next.beadID)
		}
		q.mu.Unlock()
	}
	if q.End != nil {
		q.End()
	}
}

// Wait returns nil once the queue holds no job, queued or running, and ctx's
// error when ctx ends first. An empty queue returns at once.
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
