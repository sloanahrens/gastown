package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// signalWait bounds every wait in these tests: a goroutine that never reaches
// its signal fails the test with a message instead of hanging it.
const signalWait = 10 * time.Second

// signalTestDaemon is shutdownTestDaemon with a logger the test can read.
func signalTestDaemon(t *testing.T) (*Daemon, *lockedBuffer) {
	t.Helper()
	d := shutdownTestDaemon(t)
	buf := &lockedBuffer{}
	d.logger = log.New(buf, "", 0)
	return d, buf
}

// within fails the test unless result delivers before the bound.
func within[T any](t *testing.T, bound time.Duration, what string, result <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	select {
	case v := <-result:
		return v
	case <-ctx.Done():
		t.Fatalf("%s: %s did not finish within %s", t.Name(), what, bound)
		panic("unreachable")
	}
}

// blockUntil is the wait of a fake heartbeat running in its own goroutine,
// where t.Fatalf is not allowed: it returns when ch delivers or the bound
// passes, and reports the pass as an error.
func blockUntil(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), signalWait)
	defer cancel()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Errorf("%s: the fake heartbeat waited %s for %s", t.Name(), signalWait, what)
	}
}

// reached waits for the fake heartbeat to say it is running.
func reached(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	within(t, signalWait, what, ch)
}

// A SIGTERM during a heartbeat step that stops on the daemon context shuts the
// daemon down within the limit, with the state recorded as not running. The
// grace is far longer than the limit, so only the context cancel can end the
// wait in time (gt-lbqgz).
func TestSignalDuringBlockedHeartbeatShutsDownWithinLimit(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	d.hbGrace = time.Hour
	state := &State{Running: true}
	sigChan := make(chan os.Signal, 1)
	running := make(chan struct{})
	run := func() {
		d.setHeartbeatStep("plugins")
		close(running)
		blockUntil(t, "the daemon context to be canceled", d.ctx.Done())
	}

	result := make(chan error, 1)
	go func() {
		stop, err := d.stopOnSignal(sigChan, state, run)
		if !stop {
			result <- errors.New("stopOnSignal did not stop")
			return
		}
		result <- err
	}()
	reached(t, "the heartbeat step", running)
	sigChan <- syscall.SIGTERM

	if err := within(t, 5*time.Second, "shutdown after SIGTERM", result); err != nil && !errors.Is(err, ErrUnrequestedStop) {
		t.Fatalf("%s: shutdown: %v", t.Name(), err)
	}
	if state.Running {
		t.Errorf("%s: state.Running is still true after the shutdown", t.Name())
	}
	saved, err := LoadState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("%s: LoadState: %v", t.Name(), err)
	}
	if saved.Running {
		t.Errorf("%s: the saved state still says the daemon is running", t.Name())
	}
	if strings.Contains(logs.String(), "did not stop") {
		t.Errorf("%s: a step that stops on the canceled context was reported as abandoned:\n%s", t.Name(), logs)
	}
}

// A step that ignores the cancel is abandoned after the grace, and the log
// names it: the signal line comes at once, the abandoned step after the grace.
func TestSignalDuringHeartbeatLogsAbandonedStep(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	d.hbGrace = 20 * time.Millisecond
	sigChan := make(chan os.Signal, 1)
	running := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	run := func() {
		d.setHeartbeatStep("jev-rules")
		close(running)
		blockUntil(t, "the release", release)
	}

	result := make(chan os.Signal, 1)
	go func() { result <- d.runHeartbeat(sigChan, run) }()
	reached(t, "the heartbeat step", running)
	sigChan <- syscall.SIGTERM

	if sig := within(t, 5*time.Second, "runHeartbeat after SIGTERM", result); sig != syscall.SIGTERM {
		t.Fatalf("%s: runHeartbeat returned %v, want SIGTERM", t.Name(), sig)
	}
	got := logs.String()
	for _, want := range []string{
		`Received signal terminated during heartbeat step "jev-rules", shutting down`,
		`Heartbeat step "jev-rules" did not stop within 20ms of the cancel`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s: log lacks %q:\n%s", t.Name(), want, got)
		}
	}
	if d.ctx.Err() == nil {
		t.Errorf("%s: the daemon context was not canceled by the signal", t.Name())
	}
}

// A SIGTERM outside a heartbeat is answered as before: the signal line, the
// shutdown, the state recorded as stopped.
func TestSignalWhileIdleUnchanged(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	state := &State{Running: true}

	if err := d.shutdownForSignal(syscall.SIGTERM, state, make(chan os.Signal)); err != nil && !errors.Is(err, ErrUnrequestedStop) {
		t.Fatalf("%s: shutdownForSignal: %v", t.Name(), err)
	}
	if state.Running {
		t.Errorf("%s: state.Running is still true", t.Name())
	}
	if !strings.Contains(logs.String(), "Received signal terminated, shutting down") {
		t.Errorf("%s: the signal line is missing:\n%s", t.Name(), logs)
	}
}

// SIGUSR1 during a heartbeat is ignored as the idle loop ignores it, and the
// heartbeat finishes on its own.
func TestHeartbeatIgnoresLifecycleSignalAndFinishes(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	sigChan := make(chan os.Signal, 1)
	running := make(chan struct{})
	finish := make(chan struct{})
	run := func() {
		close(running)
		blockUntil(t, "the test to let the heartbeat finish", finish)
	}

	result := make(chan os.Signal, 1)
	go func() { result <- d.runHeartbeat(sigChan, run) }()
	reached(t, "the heartbeat", running)
	sigChan <- syscall.SIGUSR1
	// The wait logs the ignored signal when it has read it; let the heartbeat go
	// only after that line is there.
	drained, cancel := context.WithTimeout(context.Background(), signalWait)
	defer cancel()
	for !strings.Contains(logs.String(), "Received SIGUSR1: ignored") {
		if drained.Err() != nil {
			t.Fatalf("%s: the wait never logged SIGUSR1 as ignored", t.Name())
		}
		runtime.Gosched()
	}
	close(finish)

	if sig := within(t, signalWait, "runHeartbeat", result); sig != nil {
		t.Fatalf("%s: runHeartbeat returned %v for a finished heartbeat", t.Name(), sig)
	}
	if !strings.Contains(logs.String(), "Received SIGUSR1: ignored") {
		t.Errorf("%s: SIGUSR1 was not logged as ignored:\n%s", t.Name(), logs)
	}
	if d.ctx.Err() != nil {
		t.Errorf("%s: an ignored signal canceled the daemon context", t.Name())
	}
}

// stopOnSignal is what Run's two heartbeat sites call: a heartbeat that ends
// by itself does not stop the daemon, and one ended by a shutdown signal
// returns the shutdown's result with the state recorded as stopped.
func TestStopOnSignalWiring(t *testing.T) {
	t.Parallel()
	t.Run("a finished heartbeat does not stop", func(t *testing.T) {
		t.Parallel()
		d, _ := signalTestDaemon(t)
		stop, err := d.stopOnSignal(make(chan os.Signal), &State{Running: true}, func() {})
		if stop || err != nil {
			t.Errorf("%s: stop=%v err=%v for a heartbeat that finished", t.Name(), stop, err)
		}
	})
	t.Run("a signal stops and shuts down", func(t *testing.T) {
		t.Parallel()
		d, _ := signalTestDaemon(t)
		sigChan := make(chan os.Signal, 1)
		sigChan <- syscall.SIGTERM
		state := &State{Running: true}
		stop, err := d.stopOnSignal(sigChan, state, func() { blockUntil(t, "the daemon context", d.ctx.Done()) })
		if !stop || (err != nil && !errors.Is(err, ErrUnrequestedStop)) {
			t.Errorf("%s: stop=%v err=%v after SIGTERM", t.Name(), stop, err)
		}
		if state.Running {
			t.Errorf("%s: the state still says the daemon is running", t.Name())
		}
	})
}

// A heartbeat that outlives the cancel starts no further step and writes no
// state: shutdown has recorded the daemon as stopped, and anything the late
// heartbeat does to the state must not change what shutdown saved.
func TestAbandonedHeartbeatDoesNotChangeWhatShutdownSaved(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	state := &State{Running: true}
	inStep := make(chan struct{})
	release := make(chan struct{})
	var steps atomic.Int32
	d.seams.heartbeatStep = func(_ *Daemon, _ heartbeatStep) {
		if steps.Add(1) == 1 {
			close(inStep)
			blockUntil(t, "the release", release)
		}
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		d.heartbeatWork(state, true)
	}()
	reached(t, "the first heartbeat step", inStep)

	// The signal path: cancel, then shutdown records the daemon as stopped.
	d.cancel()
	if err := d.shutdown(state); err != nil {
		t.Fatalf("%s: shutdown: %v", t.Name(), err)
	}
	before, err := LoadState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("%s: LoadState after shutdown: %v", t.Name(), err)
	}

	// The abandoned heartbeat wakes up and runs to its end.
	close(release)
	within(t, signalWait, "the abandoned heartbeat", finished)

	if got := steps.Load(); got != 1 {
		t.Errorf("%s: %d steps ran, want 1: a step started after the cancel", t.Name(), got)
	}
	after, err := LoadState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("%s: LoadState after the abandoned heartbeat: %v", t.Name(), err)
	}
	if after.Running || after.HeartbeatCount != before.HeartbeatCount || !after.LastHeartbeat.Equal(before.LastHeartbeat) {
		t.Errorf("%s: state changed after shutdown saved it: before %+v, after %+v", t.Name(), before, after)
	}
	if !strings.Contains(logs.String(), "Heartbeat stopped before step") {
		t.Errorf("%s: the stopped heartbeat was not logged:\n%s", t.Name(), logs)
	}
}

// A heartbeat started after the cancel runs no step and saves no state.
func TestSignalHeartbeatAfterCancelDoesNotSaveState(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	var steps atomic.Int32
	d.seams.heartbeatStep = func(*Daemon, heartbeatStep) { steps.Add(1) }
	d.cancel()
	state := &State{Running: true}

	d.heartbeatWork(state, true)

	if steps.Load() != 0 {
		t.Errorf("%s: %d steps ran after the cancel, want 0", t.Name(), steps.Load())
	}
	if state.HeartbeatCount != 0 {
		t.Errorf("%s: HeartbeatCount = %d after a canceled heartbeat, want 0", t.Name(), state.HeartbeatCount)
	}
	// Nothing was saved, so there is no state file to load; any other failure
	// is a broken read and fails the test.
	saved, err := LoadState(d.config.TownRoot)
	switch {
	case err == nil && saved.HeartbeatCount != 0:
		t.Errorf("%s: a canceled heartbeat saved HeartbeatCount %d", t.Name(), saved.HeartbeatCount)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		t.Fatalf("%s: LoadState: %v", t.Name(), err)
	}
}

// The grace a shutdown signal gives an uninterruptible step is five seconds
// (constant heartbeatSignalGrace); the hbGrace test seam overrides it, and only
// it.
func TestSignalGraceIsFiveSeconds(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	if got := d.signalGrace(); got != 5*time.Second {
		t.Errorf("%s: grace = %s, want 5s", t.Name(), got)
	}
	if heartbeatSignalGrace != 5*time.Second {
		t.Errorf("%s: heartbeatSignalGrace = %s, want 5s", t.Name(), heartbeatSignalGrace)
	}
	d.hbGrace = 7 * time.Millisecond
	if got := d.signalGrace(); got != 7*time.Millisecond {
		t.Errorf("%s: grace with the seam set = %s, want 7ms", t.Name(), got)
	}
}

// Shutdown saves a copy of the state taken under stateMu, so code that mutates
// the shared state under that lock while shutdown runs cannot change what is
// saved or race it (run under -race).
func TestShutdownSavesACopyOfTheStateUnderTheLock(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	state := &State{Running: true}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			d.hb.stateMu.Lock()
			state.HeartbeatCount++
			d.hb.stateMu.Unlock()
		}
	}()

	if err := d.shutdown(state); err != nil {
		t.Fatalf("%s: shutdown: %v", t.Name(), err)
	}
	close(stop)
	within(t, signalWait, "the writer goroutine", done)

	saved, err := LoadState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("%s: LoadState: %v", t.Name(), err)
	}
	if saved.Running {
		t.Errorf("%s: the saved state says the daemon is running", t.Name())
	}
}
