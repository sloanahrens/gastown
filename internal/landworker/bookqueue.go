package landworker

import "sync"

// BookQueue runs bookkeeping jobs one at a time, in the order they were added,
// on a goroutine of its own; the zero value is ready to use.
type BookQueue struct {
	mu     sync.Mutex
	jobs   []bookJob
	held   map[string]int // bead -> jobs added and not yet returned
	active bool           // the drain goroutine is running
}

type bookJob struct {
	beadID string
	job    func()
}

// Add queues job as bookkeeping for beadID and returns without waiting for a
// running job; Holds reports beadID from here until job returns.
func (q *BookQueue) Add(beadID string, job func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.held == nil {
		q.held = map[string]int{}
	}
	q.held[beadID]++
	q.jobs = append(q.jobs, bookJob{beadID: beadID, job: job})
	if !q.active {
		q.active = true
		go q.drain()
	}
}

// Holds reports whether beadID's bookkeeping is queued or running: true from
// its Add until its job returns, false after.
func (q *BookQueue) Holds(beadID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held[beadID] > 0
}

// drain runs the jobs one at a time and exits once none is left. The lock
// never covers a job, so Add and Holds answer while one runs; clearing active
// under the same lock Add takes loses no Add racing the last job.
func (q *BookQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.active = false
			q.mu.Unlock()
			return
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
}
