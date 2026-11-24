package daemon

import (
	"context"
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

// gt-lzw73j: the wait makes the gc patient, not the daemon's Dolt tasks. A gc
// that waited out a task's read side then holds the write side for its call,
// and inside that call the tasks' guard still refuses at once: ok false, no
// release handed back, and not one sleep on the wait's clock. So a task tick
// landing on a long gc ends in a skip, never in a wait of its own.
func TestGCLockWaitTasksStillSkip(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DoctorDog: &DoctorDogConfig{Enabled: true},
	}}
	c := &gcLockWaitClock{now: gcTestNow}
	d.maint.now, d.maint.sleep = c.Now, c.Sleep

	// A task holds the read side first, so the gc's wait runs; it releases on
	// the third poll and the gc takes the write side.
	release, ok := d.tryDoltTask("jsonl_git_backup")
	if !ok {
		t.Fatal("could not take the read side")
	}
	c.at, c.release = 3, release

	var skippedOK bool
	var skippedRelease func()
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		// The real caller skips its tick before it probes Dolt.
		d.runDoctorDog()
		// And the guard's contract: refused, with no release handed back.
		skippedRelease, skippedOK = d.tryDoltTask("wisp_reaper")
		return nil
	}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeCompleted || len(f.gcCalls) != 1 {
		t.Fatalf("outcome = %v (%s), gc calls %v, want completed with one gc", out.outcome, out.reason, f.gcCalls)
	}
	if skippedOK {
		t.Error("a task took the read side while the gc held the write side")
	}
	if skippedRelease != nil {
		t.Error("a skipped task was handed a release func")
	}
	if c.polls != 3 {
		t.Errorf("polls = %d, want 3: the gc's own wait only, no task slept on it", c.polls)
	}
	if !strings.Contains(logs.String(), "doctor_dog: skipped: gc in flight") {
		t.Errorf("doctor_dog's tick did not skip:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "all clear") || strings.Contains(logs.String(), "finding(s)") {
		t.Errorf("doctor_dog probed Dolt while the gc held the write side:\n%s", logs.String())
	}
}
