package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// outboxPollRun is a Daemon.Run under test with the outbox poll's ticker
// injected: the test sends ticks into the select loop through ticks, reads the
// interval Run asked the ticker seam for from asked, and ends Run with stop
// (gt-dyu7re).
type outboxPollRun struct {
	d     *Daemon
	ticks chan time.Time
	asked chan time.Duration
	stops *atomic.Int32
	done  chan error
}

// startOutboxPollRun runs d.Run in a goroutine against a town whose outbox_poll
// patrol is enabled and whose other host-touching startup is stubbed, as the
// census Run test does, and returns once the goroutine has started. The caller
// ends it with stop.
func startOutboxPollRun(t *testing.T) *outboxPollRun {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		config:       DefaultConfig(town),
		logger:       log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{OutboxPoll: &agentconfig.OutboxPollConfig{Enabled: true}}},
	}
	// The rest of Run's startup is out of scope, and each of these does real
	// host work: the heartbeat dispatches and restarts sessions, the sweep
	// touches the tmux socket directory, and the probes reach the network.
	d.disabledPatrols = map[string]bool{
		"landing_infra":    true,
		"resource_sampler": true,
		"spec_dispatch":    true,
	}
	d.seams.heartbeatWork = func(*Daemon, *State, bool) {}
	d.seams.cleanupLegacySockets = func(string) (int, int) { return 0, 0 }

	r := &outboxPollRun{
		d:     d,
		ticks: make(chan time.Time),
		asked: make(chan time.Duration, 1),
		stops: new(atomic.Int32),
		done:  make(chan error, 1),
	}
	d.newOutboxPollTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		r.asked <- interval
		return r.ticks, func() { r.stops.Add(1) }
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	t.Cleanup(d.cancel)
	go func() { r.done <- d.Run() }()
	return r
}

// stop ends Run through the daemon context and fails unless it returns nil
// within the bound.
func (r *outboxPollRun) stop(t *testing.T) {
	t.Helper()
	r.d.cancel()
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Run after the daemon context ended = %v, want a clean shutdown", err)
		}
	case <-bounded.Done():
		t.Fatal("Run did not return after the daemon context ended")
	}
}

// TestNoBeadAccessOutboxPollRunAsksForItsTicker pins the call site that gives
// the outbox poll its ticker: Run asks the ticker seam once, for
// defaultOutboxPollInterval, and returns nil when the daemon context ends
// (gt-dyu7re).
func TestNoBeadAccessOutboxPollRunAsksForItsTicker(t *testing.T) {
	t.Parallel()
	r := startOutboxPollRun(t)
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case got := <-r.asked:
		if got != defaultOutboxPollInterval {
			t.Errorf("Run asked for a ticker of %v, want the %v default", got, defaultOutboxPollInterval)
		}
	case <-bounded.Done():
		t.Fatal("Run never asked for the outbox poll's ticker: the start path does not wire it in")
	}
	r.stop(t)
	if got := len(r.asked); got != 0 {
		t.Errorf("Run asked for the ticker %d more times, want once", got)
	}
}
