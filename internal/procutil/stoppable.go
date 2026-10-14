package procutil

import (
	"context"
	"time"
)

// Stoppable runs a loop of polls, each of which starts child processes and
// Waits for them, so that the process can stop the loop and have every child
// reaped before it replaces its own image with syscall.Exec.
//
// Exec leaves this process's children in place, and the new image has no
// goroutine that Waits for them: each one that exits afterwards stays defunct
// under the same pid until that process exits. The dashboard's hub has about ten
// bd and git reads in flight at any moment, so an in-place restart stranded
// about ten per install (gt-9rwye). Stopping the loop and waiting out the polls
// in flight lets each poll's own Wait reap its child first.
//
// Start and Quiesce are called from one goroutine at a time.
type Stoppable struct {
	parent context.Context
	run    func(context.Context)
	cancel context.CancelFunc
	done   chan struct{}

	// newTicker makes the clock that repeats the kill during Quiesce; tests
	// replace it with a channel they feed.
	newTicker func(time.Duration) (ticks <-chan time.Time, stop func())
}

// killInterval is how often Quiesce kills the stragglers' children again while
// it waits for them to return. One pass is not enough: a poll that starts
// several children in sequence starts its next one after the pass, and that
// child would still be running, and so stranded, when the process execs
// (gt-6f0e8).
const killInterval = 200 * time.Millisecond

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// QuiesceBounds are the two waits of Stoppable.Quiesce.
type QuiesceBounds struct {
	Drain time.Duration // for the polls in flight to return unaided
	Kill  time.Duration // for them to return once their children are killed
}

// StartStoppable starts run under a child of parent and returns the handle that
// stops it. run must return once its context ends and the polls it started have
// returned; it may be started again after it has.
func StartStoppable(parent context.Context, run func(context.Context)) *Stoppable {
	s := &Stoppable{parent: parent, run: run, newTicker: realTicker}
	s.start()
	return s
}

func (s *Stoppable) start() {
	ctx, cancel := context.WithCancel(s.parent)
	done := make(chan struct{})
	s.cancel, s.done = cancel, done
	go func() {
		defer close(done)
		s.run(ctx)
	}()
}

// Quiesce stops the loop from starting polls and waits for the ones in flight to
// return. A poll still running after b.Drain is a straggler: killChildren kills
// the child processes it is waiting on, which lets its Wait return and reap
// them. Quiesce then waits up to b.Kill more, killing again every killInterval
// so a child the poll starts after a pass is killed too. It returns when the
// loop has returned or both bounds have run out, and says through logf when it
// killed (with the total over all passes) or gave up. The returned resume starts the loop again, for a restart whose exec
// failed and left this process serving; it does nothing once parent has ended.
func (s *Stoppable) Quiesce(b QuiesceBounds, killChildren func() int, logf func(format string, args ...any)) (resume func()) {
	s.cancel()
	resume = func() {
		if s.parent.Err() == nil {
			s.start()
		}
	}
	if waitFor(s.done, b.Drain) {
		return resume
	}
	killed := killChildren()
	returned := s.killUntilReturned(b.Kill, func() { killed += killChildren() })
	logf("polls still running %s into the restart; killed %d child process(es)", b.Drain, killed)
	if !returned {
		logf("polls still running %s after the kill; restarting anyway", b.Kill)
	}
	return resume
}

// killUntilReturned calls kill every killInterval until the loop has returned
// or d has passed, and reports whether the loop returned.
func (s *Stoppable) killUntilReturned(d time.Duration, kill func()) bool {
	ticks, stopTicks := s.newTicker(killInterval)
	defer stopTicks()
	bound := time.NewTimer(d)
	defer bound.Stop()
	for {
		// A return that races a tick wins: nothing is left to kill.
		select {
		case <-s.done:
			return true
		default:
		}
		select {
		case <-s.done:
			return true
		case <-bound.C:
			return false
		case <-ticks:
			kill()
		}
	}
}

// waitFor reports whether done closed within d.
func waitFor(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
