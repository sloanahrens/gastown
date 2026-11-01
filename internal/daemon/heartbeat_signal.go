package daemon

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// heartbeatSignalGrace is how long a shutdown signal waits for the running
// heartbeat step to stop on the cancelled daemon context before the daemon
// shuts down around it (gt-lbqgz). A heartbeat takes 80 to 160 s under load;
// the main loop used to read its signal channel only between heartbeats, so a
// SIGTERM waited that long and launchd's 30 s stop ended in SIGKILL.
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

// runHeartbeat runs one heartbeat (run) beside the signal channel, so a
// shutdown signal is read while the heartbeat is in flight. It returns nil
// when the heartbeat finished, and the signal when a shutdown signal arrived
// first. On that signal it logs at once, names the step in flight, cancels the
// daemon context every step and patrol stops on, and waits at most the grace
// for the heartbeat to come back; a step that does not is abandoned and
// named, and the caller shuts the daemon down around it. SIGUSR1 and the
// reload-restart signal are answered as the main loop answers them and do not
// end the wait.
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

// heartbeatCancelled reports whether the daemon context is cancelled, which
// is how a heartbeat that outlived its shutdown learns to stop writing.
func (d *Daemon) heartbeatCancelled() bool {
	return d.ctx != nil && d.ctx.Err() != nil
}
