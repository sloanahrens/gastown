package daemon

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// heartbeatSignalGrace bounds how long a shutdown signal waits for a heartbeat
// step that does not stop on the canceled daemon context (gt-lbqgz). It stays
// well inside launchd's 30 s stop.
const heartbeatSignalGrace = 5 * time.Second

// heartbeatRun is the state one heartbeat shares with the signal wait: the
// name of the step in flight and the lock that keeps an abandoned heartbeat
// from writing the daemon state after shutdown has.
type heartbeatRun struct {
	step    atomic.Pointer[string]
	stateMu sync.Mutex
}

// setHeartbeatStep records the step the heartbeat is in ("" between steps), so
// a signal that arrives mid-step can name the step it abandons.
func (d *Daemon) setHeartbeatStep(name string) {
	if name == "" {
		d.hb.step.Store(nil)
		return
	}
	d.hb.step.Store(&name)
}

// heartbeatStepName is the step in flight, or "" between steps.
func (d *Daemon) heartbeatStepName() string {
	if p := d.hb.step.Load(); p != nil {
		return *p
	}
	return ""
}

// runHeartbeat runs one heartbeat (run) beside the signal channel. It returns
// nil when the heartbeat finished and the signal when a shutdown signal came
// first. On that signal it logs at once, names the step in flight, cancels the
// daemon context, and waits at most the grace for the heartbeat; a step that
// does not stop is named and abandoned. An abandoned heartbeat starts no
// further step and writes no state (heartbeatWork checks the canceled context
// before each step and under stateMu). SIGUSR1 and the reload-restart signal
// are answered as the main loop answers them and do not end the wait.
func (d *Daemon) runHeartbeat(sigChan <-chan os.Signal, run func()) os.Signal {
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	for {
		select {
		case <-done:
			return nil
		case sig := <-sigChan:
			if isLifecycleSignal(sig) {
				d.logger.Println("Received SIGUSR1: ignored")
				continue
			}
			if isReloadRestartSignal(sig) {
				d.logger.Println("Received reload-restart signal: restart budgets are read from the intent records, nothing to reload")
				continue
			}
			d.logSignalDuringHeartbeat(sig)
			if d.cancel != nil {
				d.cancel()
			}
			grace := d.hbGrace
			if grace <= 0 {
				grace = heartbeatSignalGrace
			}
			wait := time.NewTimer(grace)
			defer wait.Stop()
			select {
			case <-done:
			case <-wait.C:
				d.logger.Printf("Heartbeat step %q did not stop within %s of the cancel; shutting down without it", d.heartbeatStepName(), grace)
			}
			return sig
		}
	}
}

// logSignalDuringHeartbeat writes the signal line the main loop writes, with
// the step it interrupts when a heartbeat is mid-step.
func (d *Daemon) logSignalDuringHeartbeat(sig os.Signal) {
	if step := d.heartbeatStepName(); step != "" {
		d.logger.Printf("Received signal %v during heartbeat step %q, shutting down", sig, step)
		return
	}
	d.logger.Printf("Received signal %v during a heartbeat, shutting down", sig)
}

// heartbeatCanceled reports whether the daemon context is canceled, which
// is how a heartbeat that outlived its shutdown learns to stop writing.
func (d *Daemon) heartbeatCanceled() bool {
	return d.ctx != nil && d.ctx.Err() != nil
}

// stopOnSignal runs one heartbeat beside sigChan and, when a shutdown signal
// ended it, runs the shutdown. stop is true when the daemon must return exit,
// the shutdown's result.
// Both heartbeat call sites in Run use it.
func (d *Daemon) stopOnSignal(sigChan <-chan os.Signal, state *State, run func()) (stop bool, exit error) {
	if sig := d.runHeartbeat(sigChan, run); sig != nil {
		return true, d.finishSignalShutdown(state)
	}
	return false, nil
}
