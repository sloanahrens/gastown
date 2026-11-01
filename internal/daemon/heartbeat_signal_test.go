package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
		t.Fatalf("%s did not finish within %s", what, bound)
		panic("unreachable")
	}
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
		<-d.ctx.Done()
	}

	result := make(chan error, 1)
	go func() {
		sig := d.runHeartbeat(sigChan, run)
		if sig == nil {
			result <- errors.New("runHeartbeat returned no signal")
			return
		}
		result <- d.finishSignalShutdown(state)
	}()
	<-running
	sigChan <- syscall.SIGTERM

	if err := within(t, 5*time.Second, "shutdown after SIGTERM", result); err != nil && !errors.Is(err, ErrUnrequestedStop) {
		t.Fatalf("shutdown: %v", err)
	}
	if state.Running {
		t.Error("state.Running is still true after the shutdown")
	}
	saved, err := LoadState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if saved.Running {
		t.Error("the saved state still says the daemon is running")
	}
	if strings.Contains(logs.String(), "did not stop") {
		t.Errorf("a step that stops on the canceled context was reported as abandoned:\n%s", logs)
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
		<-release
	}

	result := make(chan os.Signal, 1)
	go func() { result <- d.runHeartbeat(sigChan, run) }()
	<-running
	sigChan <- syscall.SIGTERM

	if sig := within(t, 5*time.Second, "runHeartbeat after SIGTERM", result); sig != syscall.SIGTERM {
		t.Fatalf("runHeartbeat returned %v, want SIGTERM", sig)
	}
	got := logs.String()
	for _, want := range []string{
		`Received signal terminated during heartbeat step "jev-rules", shutting down`,
		`Heartbeat step "jev-rules" did not stop within 20ms of the cancel`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if d.ctx.Err() == nil {
		t.Error("the daemon context was not canceled by the signal")
	}
}

// A SIGTERM outside a heartbeat is answered as before: the signal line, the
// shutdown, the state recorded as stopped.
func TestSignalWhileIdleUnchanged(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	state := &State{Running: true}

	if err := d.shutdownForSignal(syscall.SIGTERM, state); err != nil && !errors.Is(err, ErrUnrequestedStop) {
		t.Fatalf("shutdownForSignal: %v", err)
	}
	if state.Running {
		t.Error("state.Running is still true")
	}
	if !strings.Contains(logs.String(), "Received signal terminated, shutting down") {
		t.Errorf("the signal line is missing:\n%s", logs)
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
		<-finish
	}

	result := make(chan os.Signal, 1)
	go func() { result <- d.runHeartbeat(sigChan, run) }()
	<-running
	sigChan <- syscall.SIGUSR1
	// Let the wait read the signal before the heartbeat is let go: the
	// buffered channel is empty once it has.
	drained, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for len(sigChan) > 0 {
		if drained.Err() != nil {
			t.Fatal("the wait never read SIGUSR1")
		}
		runtime.Gosched()
	}
	close(finish)

	if sig := within(t, 5*time.Second, "runHeartbeat", result); sig != nil {
		t.Fatalf("runHeartbeat returned %v for a finished heartbeat", sig)
	}
	if !strings.Contains(logs.String(), "Received SIGUSR1: ignored") {
		t.Errorf("SIGUSR1 was not logged as ignored:\n%s", logs)
	}
	if d.ctx.Err() != nil {
		t.Error("an ignored signal canceled the daemon context")
	}
}

// A heartbeat that outlives the cancel writes no state: shutdown has recorded
// the daemon as stopped and a late save would undo it.
func TestHeartbeatAfterCancelDoesNotSaveState(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	d.seams.heartbeatStep = func(*Daemon, heartbeatStep) {}
	d.cancel()
	state := &State{Running: true}

	d.heartbeatWork(state, true)

	if state.HeartbeatCount != 0 {
		t.Errorf("HeartbeatCount = %d after a canceled heartbeat, want 0", state.HeartbeatCount)
	}
	if saved, err := LoadState(d.config.TownRoot); err == nil && saved.HeartbeatCount != 0 {
		t.Errorf("a canceled heartbeat saved HeartbeatCount %d", saved.HeartbeatCount)
	}
}
