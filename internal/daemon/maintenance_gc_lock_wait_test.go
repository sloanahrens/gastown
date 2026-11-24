package daemon

import (
	"strings"
	"testing"
	"time"
)

// gcLockWaitClock is the cycle's clock and sleep in the lock-wait tests: Sleep
// advances the fake time and can release a held read lock on a chosen poll, so
// the wait runs without real time passing.
type gcLockWaitClock struct {
	now     time.Time
	polls   int
	at      int
	release func()
}

func (c *gcLockWaitClock) Now() time.Time { return c.now }

func (c *gcLockWaitClock) Sleep(d time.Duration) {
	c.now = c.now.Add(d)
	c.polls++
	if c.polls == c.at {
		c.release()
	}
}

// gt-u15nxv: a Dolt task on the tick's schedule gets the wait's minute to
// release the read side, and the gc then runs in this tick.
func TestGCLockWaitAcquiresWhenReleased(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	c := &gcLockWaitClock{now: gcTestNow}
	d.maint.now, d.maint.sleep = c.Now, c.Sleep

	// lockWithin itself: the task releases on the third poll.
	release, ok := d.tryDoltTask("jsonl_git_backup")
	if !ok {
		t.Fatal("could not take the read side")
	}
	c.at, c.release = 3, release
	if !d.lockWithin(c.Now, c.Sleep) {
		t.Fatal("lockWithin = false, want true: the task released on the third poll")
	}
	d.doltMaintMu.Unlock()

	// The cycle calls it: the gc runs in this tick.
	c = &gcLockWaitClock{now: gcTestNow}
	d.maint.now, d.maint.sleep = c.Now, c.Sleep
	release, ok = d.tryDoltTask("jsonl_git_backup")
	if !ok {
		t.Fatal("could not take the read side for the cycle")
	}
	c.at, c.release = 3, release

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeCompleted || strings.Join(f.gcCalls, ",") != "hq" {
		t.Fatalf("outcome %v (%s), gc calls %v, want completed with hq", out.outcome, out.reason, f.gcCalls)
	}
	if c.polls != 3 {
		t.Errorf("the cycle polled %d times, want 3: the gc did not wait for the task", c.polls)
	}
}

// gt-u15nxv: a task that holds the read side through the whole wait makes the
// gc defer after it, not run beside it.
func TestGCLockWaitTimesOut(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	c := &gcLockWaitClock{now: gcTestNow}
	d.maint.now, d.maint.sleep = c.Now, c.Sleep
	release, ok := d.tryDoltTask("jsonl_git_backup")
	if !ok {
		t.Fatal("could not take the read side")
	}
	defer release()

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeDeferred || out.reason != "daemon Dolt task in flight" {
		t.Fatalf("result = %v/%q, want deferred/'daemon Dolt task in flight'", out.outcome, out.reason)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran while the task held the read side: %v", f.gcCalls)
	}
	if got := c.Now().Sub(gcTestNow); got != maintenanceGCLockWait {
		t.Errorf("the wait ended after %v of fake time, want %v", got, maintenanceGCLockWait)
	}
	if c.polls != maintenanceGCLockPolls {
		t.Errorf("polls = %d, want %d", c.polls, maintenanceGCLockPolls)
	}
}
