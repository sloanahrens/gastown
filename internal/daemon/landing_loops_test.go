package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landworker"
)

// landingLoopJoinTimeout bounds a fixture's wait for its loops to return. A
// loop that outlives its canceled context fails the test rather than hanging
// the package.
const landingLoopJoinTimeout = 10 * time.Second

// landingLoopSet owns the per-rig worker loops a test starts against its
// fixture's daemon. The fixture cancels the daemon's context and then waits on
// the set, so no pass is still writing patrol_last_run.json while t.TempDir
// removes the town (gt-zv3ndb).
type landingLoopSet struct {
	d  *Daemon
	mu sync.Mutex
	// done holds one channel per started loop, closed when that loop returns.
	done []chan struct{}
}

// start runs one rig's worker loop on its own goroutine and records it for the
// fixture's teardown. Start every loop a test needs through the set: a loop
// started some other way is not waited for.
func (s *landingLoopSet) start(rigName string, interval time.Duration, pass func(context.Context) landworker.Report) {
	done := make(chan struct{})
	s.mu.Lock()
	s.done = append(s.done, done)
	s.mu.Unlock()
	go func() {
		defer close(done)
		s.d.landingWorkerLoop(rigName, interval, pass)
	}()
}

// wait blocks until every loop started on the set has returned, or fails the
// test when one has not within landingLoopJoinTimeout. The caller cancels the
// daemon's context first: the loops exit through it.
func (s *landingLoopSet) wait(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	started := append([]chan struct{}(nil), s.done...)
	s.mu.Unlock()
	timeout := time.After(landingLoopJoinTimeout)
	for _, done := range started {
		select {
		case <-done:
		case <-timeout:
			t.Error("a landing worker loop did not return after its daemon's context was canceled")
			return
		}
	}
}
