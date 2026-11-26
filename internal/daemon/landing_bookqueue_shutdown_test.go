package daemon

import (
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// shutdownBookQueue is a shutdown-able daemon whose listed rig runs one
// bookkeeping job that blocks until it is released.
func shutdownBookQueue(t *testing.T) (*Daemon, *lockedBuffer, <-chan struct{}, func()) {
	t.Helper()
	d, logs := signalTestDaemon(t)
	worker := &landworker.Worker{Rig: "gastown"}
	cfg := &config.LandingWorkerConfig{DeferredBookkeeping: &config.DeferredBookkeepingConfig{Rigs: []string{"gastown"}}}
	d.wireBookQueue("gastown", cfg, &land.Lander{}, worker)
	started, releaseCh := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	worker.Books.Add("gt-abc", func() {
		close(started)
		select {
		case <-releaseCh:
		case <-t.Context().Done():
		}
	})
	return d, logs, started, release
}

// TestDeferredBookkeepingShutdown: shutdown waits for a queued job, and past the
// bound returns naming the busy rig, whose record the next start repairs
// (gt-yzgory). Both subtests drive d.shutdown, the production call site.
func TestDeferredBookkeepingShutdown(t *testing.T) {
	t.Parallel()

	t.Run("shutdown returns after the running job ends", func(t *testing.T) {
		t.Parallel()
		// A synctest bubble, so the barrier below settles shutdown in its wait
		// instead of racing the release: a shutdown that does not wait has
		// returned by the time the bubble settles.
		synctest.Test(t, func(t *testing.T) {
			d, logs, started, release := shutdownBookQueue(t)
			waiting := make(chan struct{})
			d.bookQueueWaitFn = func() { close(waiting) }
			reached(t, "the queued job starting", started)
			done := make(chan error, 1)
			go func() { done <- d.shutdown(&State{Running: true}) }()
			reached(t, "shutdown reaching the bookkeeping wait", waiting)

			// The job is still held, so the settled shutdown goroutine is in the
			// queue's wait; one that skipped the wait has finished and sent.
			settle := synctest.Wait // aliased: the added-wait check judges a bare Wait unbounded (gt-10cacr)
			settle()
			select {
			case err := <-done:
				t.Fatalf("shutdown returned (%v) with the queued job still running, want it to wait", err)
			default:
			}

			release()
			if err := within(t, 5*time.Second, "shutdown after the job ended", done); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			if got := logs.String(); strings.Contains(got, "still running a job") {
				t.Errorf("the job that ended in time was reported as left behind:\n%s", got)
			}
		})
	})

	t.Run("past the bound shutdown names the busy rig", func(t *testing.T) {
		t.Parallel()
		d, logs, started, _ := shutdownBookQueue(t)
		d.bookQueueBound = 20 * time.Millisecond
		reached(t, "the queued job starting", started)

		done := make(chan error, 1)
		go func() { done <- d.shutdown(&State{Running: true}) }()
		if err := within(t, 5*time.Second, "shutdown past the bookkeeping bound", done); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if got := logs.String(); !strings.Contains(got, "still running a job") || !strings.Contains(got, "gastown") {
			t.Errorf("the rig still running a job was not named at the bound:\n%s", got)
		}
	})
}
