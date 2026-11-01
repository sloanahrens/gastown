package daemon

import (
	"sync"
	"sync/atomic"
	"time"
)

// attentionShutdownWait bounds how long shutdown waits for a running attention
// tick after it has canceled the daemon context (gt-zszj0); a tick that does
// not stop is left behind rather than holding the exit.
const attentionShutdownWait = 5 * time.Second

// attentionRun is the background attention tick: the flag that keeps one tick
// in flight, and the group shutdown waits on.
type attentionRun struct {
	running atomic.Bool
	wg      sync.WaitGroup
	// mu orders a tick's start against shutdown's wait: a tick is registered
	// with wg under mu, and waitForAttention sets stopping under mu before it
	// waits, so no tick starts once the wait has begun.
	mu       sync.Mutex
	stopping bool
	// wait overrides attentionShutdownWait in tests; tick replaces
	// writeAttention in tests.
	wait time.Duration
	tick func()
}

// startAttention is the attention heartbeat step (gt-zszj0). The tick runs
// beside the heartbeat, one at a time, under the daemon context; a heartbeat
// that finds the previous tick running starts nothing. The tick's content,
// budget and state file are writeAttention's. d.lastAttention is read and
// written by that one tick alone, which the single flight guarantees.
func (d *Daemon) startAttention() {
	d.attention.mu.Lock()
	if d.attention.stopping || d.heartbeatCanceled() {
		d.attention.mu.Unlock()
		d.logger.Println("attention: not started, the daemon is stopping")
		return
	}
	if !d.attention.running.CompareAndSwap(false, true) {
		d.attention.mu.Unlock()
		d.logger.Println("attention: previous tick still running, skipping")
		return
	}
	d.attention.wg.Add(1)
	d.attention.mu.Unlock()
	go func() {
		defer d.attention.wg.Done()
		defer d.attention.running.Store(false)
		defer func() {
			if r := recover(); r != nil {
				d.logger.Printf("attention: tick panicked: %v", r)
			}
		}()
		if d.attention.tick != nil {
			d.attention.tick()
			return
		}
		d.writeAttention()
	}()
}

// waitForAttention waits for a running attention tick for at most the shutdown
// bound and says so when it gives up. shutdown calls it after the daemon
// context is canceled, which is what ends the tick.
func (d *Daemon) waitForAttention() {
	d.attention.mu.Lock()
	d.attention.stopping = true
	d.attention.mu.Unlock()
	done := make(chan struct{})
	go func() {
		d.attention.wg.Wait()
		close(done)
	}()
	bound := d.attention.wait
	if bound <= 0 {
		bound = attentionShutdownWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("attention: tick still running %s after the cancel; shutting down without it", bound)
	}
}
