package daemon

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// attentionDrained reports, through a channel, that the attention group has finished.
func attentionDrained(d *Daemon) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		d.attention.wg.Wait()
		close(done)
	}()
	return done
}

// A heartbeat step that starts the attention tick returns at once, although
// the tick blocks until the daemon context ends (gt-zszj0).
func TestAttentionStepDoesNotHoldTheHeartbeat(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)
	started := make(chan struct{})
	d.attention.tick = func() {
		close(started)
		<-d.ctx.Done()
	}

	returned := make(chan struct{})
	go func() {
		d.startAttention()
		close(returned)
	}()
	within(t, time.Second, "the attention step", returned)
	<-started

	d.cancel()
	within(t, 5*time.Second, "the canceled tick", attentionDrained(d))
}

// Two heartbeats while the first tick runs start one tick and log the skip
// once; when it ends the next heartbeat starts a new one.
func TestAttentionStepIsSingleFlight(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	var ticks atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	d.attention.tick = func() {
		ticks.Add(1)
		started <- struct{}{}
		<-release
	}

	d.startAttention()
	<-started
	d.startAttention()
	if got := ticks.Load(); got != 1 {
		t.Fatalf("ticks started = %d while the first was running, want 1", got)
	}
	if n := strings.Count(logs.String(), "attention: previous tick still running, skipping"); n != 1 {
		t.Errorf("skip line logged %d times, want 1:\n%s", n, logs)
	}

	close(release)
	within(t, 5*time.Second, "the first tick", attentionDrained(d))
	d.startAttention()
	<-started
	if got := ticks.Load(); got != 2 {
		t.Errorf("ticks after the first ended = %d, want 2", got)
	}
	within(t, 5*time.Second, "the second tick", attentionDrained(d))
}

// A panicking tick is logged, the flag is cleared and the next heartbeat
// starts a tick.
func TestAttentionStepRecoversAPanic(t *testing.T) {
	t.Parallel()
	d, logs := signalTestDaemon(t)
	var ticks atomic.Int32
	d.attention.tick = func() {
		if ticks.Add(1) == 1 {
			panic("collector exploded")
		}
	}

	d.startAttention()
	within(t, 5*time.Second, "the panicking tick", attentionDrained(d))
	if !strings.Contains(logs.String(), "attention: tick panicked: collector exploded") {
		t.Errorf("the panic was not logged:\n%s", logs)
	}
	d.startAttention()
	within(t, 5*time.Second, "the next tick", attentionDrained(d))
	if got := ticks.Load(); got != 2 {
		t.Errorf("ticks = %d, want 2: the flag stayed set after the panic", got)
	}
}

// Shutdown cancels the context, then waits for a running tick for at most the
// bound: a tick that stops on the cancel costs nothing, one that does not is
// named and left behind.
func TestShutdownWaitsForAttentionAtMostFiveSeconds(t *testing.T) {
	t.Parallel()
	t.Run("a tick that stops on the cancel", func(t *testing.T) {
		t.Parallel()
		d, logs := signalTestDaemon(t)
		d.attention.wait = time.Hour
		started := make(chan struct{})
		d.attention.tick = func() {
			close(started)
			<-d.ctx.Done()
		}
		d.startAttention()
		<-started

		done := make(chan error, 1)
		go func() { done <- d.shutdown(&State{Running: true}) }()
		if err := within(t, 5*time.Second, "shutdown", done); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if strings.Contains(logs.String(), "tick still running") {
			t.Errorf("a tick that stopped on the cancel was reported as left behind:\n%s", logs)
		}
	})
	t.Run("a tick that ignores the cancel", func(t *testing.T) {
		t.Parallel()
		d, logs := signalTestDaemon(t)
		d.attention.wait = 30 * time.Millisecond
		started := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		d.attention.tick = func() {
			close(started)
			<-release
		}
		d.startAttention()
		<-started

		done := make(chan error, 1)
		go func() { done <- d.shutdown(&State{Running: true}) }()
		if err := within(t, 5*time.Second, "shutdown", done); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if !strings.Contains(logs.String(), "attention: tick still running 30ms after the cancel") {
			t.Errorf("the abandoned tick was not named:\n%s", logs)
		}
	})
}

// The real tick, run by the background step, ends when the daemon context is
// canceled: its budget is a child of that context (gt-zszj0 composes with
// gt-lbqgz).
func TestAttentionTickStopsOnTheDaemonContext(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.d.ctx, f.d.cancel = ctx, cancel

	reading := make(chan struct{})
	f.src.escalations = func(ctx context.Context) ([]*beads.Issue, error) {
		close(reading)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.d.attention.tick = func() {
		f.d.writeAttentionWith(time.Hour, func(time.Time) *attentionSources { return f.src })
	}

	f.d.startAttention()
	<-reading
	f.d.cancel()
	within(t, 5*time.Second, "the tick after the cancel", attentionDrained(f.d))
}
