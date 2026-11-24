package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landworker"
)

// landingLoopSet owns the per-rig worker loops a test starts against its
// fixture's daemon. The fixture cancels the daemon's context and then waits on
// the set, so no pass is still writing patrol_last_run.json while t.TempDir
// removes the town (gt-zv3ndb).
//
// The wait carries no deadline: a loop that does not return after its context
// is canceled is a bug, and the package's go test -timeout reports it with
// every stack, as the waits in landing_drain_test.go do.
type landingLoopSet struct {
	d  *Daemon
	wg sync.WaitGroup
}

// start runs one rig's worker loop on its own goroutine and joins it at the
// fixture's teardown. Start every loop a test needs through the set: a loop
// started some other way is not waited for.
func (s *landingLoopSet) start(rigName string, interval time.Duration, pass func(context.Context) landworker.Report) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.d.landingWorkerLoop(rigName, interval, pass)
	}()
}

// wait blocks until every loop started on the set has returned. The caller
// cancels the daemon's context first: the loops exit through it.
func (s *landingLoopSet) wait(t *testing.T) {
	t.Helper()
	s.wg.Wait()
}
