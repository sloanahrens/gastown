package daemon

import (
	"sync"
	"sync/atomic"
	"time"
)

// attentionShutdownWait bounds how long shutdown waits for a running attention
// tick after it has canceled the daemon context (gt-zszj0). The tick stops on
// that context; a read that does not come back is left behind rather than
// holding the exit.
const attentionShutdownWait = 5 * time.Second

// attentionRun is the background attention tick: the flag that keeps one tick
// in flight, and the group shutdown waits on.
type attentionRun struct {
	running atomic.Bool
	wg      sync.WaitGroup
	// wait overrides attentionShutdownWait in tests; tick replaces
	// writeAttention in tests.
	wait time.Duration
	tick func()
}

// startAttention is the attention heartbeat step. The tick took 75 to 86 % of
// the 92 to 114 s heartbeats measured on 2026-11-01 and the heartbeat runs on
// the main loop, so it runs beside the heartbeat instead: one tick at a time,
// under the daemon context. A heartbeat that finds the previous tick still
// running starts nothing. The tick's content, budget and state file are
// writeAttention's, unchanged; the attention cache (d.lastAttention) is read
// and written by that one tick alone, which the single flight guarantees.
func (d *Daemon) startAttention() {
	if !d.attention.running.CompareAndSwap(false, true) {
		d.logger.Println("attention: previous tick still running, skipping")
		return
	}
	d.attention.wg.Add(1)
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
