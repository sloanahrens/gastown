package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// censusLogSink is the daemon logger's writer under test: it signals every
// line, so the test waits on the log itself instead of on a timer.
type censusLogSink struct {
	mu    sync.Mutex
	lines []string
	wrote chan struct{}
}

func newCensusLogSink() *censusLogSink {
	return &censusLogSink{wrote: make(chan struct{}, 8)}
}

func (s *censusLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.lines = append(s.lines, strings.TrimSpace(string(p)))
	s.mu.Unlock()
	s.wrote <- struct{}{}
	return len(p), nil
}

// waitForLine fails unless the logger records a line containing want.
func (s *censusLogSink) waitForLine(t *testing.T, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		s.mu.Lock()
		lines := append([]string(nil), s.lines...)
		s.mu.Unlock()
		for _, line := range lines {
			if strings.Contains(line, want) {
				return
			}
		}
		select {
		case <-s.wrote:
		case <-ctx.Done():
			t.Fatalf("the daemon logger never logged %q; it logged %q", want, lines)
		}
	}
}

// countContaining counts the lines the logger received that carry substr.
func (s *censusLogSink) countContaining(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, line := range s.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// censusLogProbe stands in for beads.StartCensusLog: it names each tick it
// receives through the logf the daemon's wiring passed it, so the test sees
// both what the census was handed and where its lines land.
type censusLogProbe struct {
	started chan struct{}
	stopped chan struct{}
}

func (p *censusLogProbe) start(ctx context.Context, every <-chan time.Time, logf func(string, ...any)) {
	close(p.started)
	defer close(p.stopped)
	for {
		select {
		case <-ctx.Done():
			return
		case at, ok := <-every:
			if !ok {
				return
			}
			logf("census tick at %s", at.UTC().Format(time.RFC3339Nano))
		}
	}
}

// sendCensusTick delivers one tick, failing rather than blocking the package
// when the census has stopped receiving.
func sendCensusTick(t *testing.T, ticks chan<- time.Time, at time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case ticks <- at:
	case <-ctx.Done():
		t.Fatal("the census stopped receiving ticks")
	}
}

// TestRunStartsTheCensusLogOnMinuteTicks pins the daemon's census wiring
// (gt-7h3vm) at the call site that gives it life: a Run start asks for a
// one-minute ticker, hands the census the daemon's own context and logger,
// logs one line per tick through that logger, and stops the ticker when Run
// returns.
func TestRunStartsTheCensusLogOnMinuteTicks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := newCensusLogSink()
	d := &Daemon{config: DefaultConfig(town), logger: log.New(sink, "", 0)}
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

	ticks := make(chan time.Time)
	var intervals []time.Duration
	stops := 0
	d.seams.censusTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}
	probe := &censusLogProbe{started: make(chan struct{}), stopped: make(chan struct{})}
	d.seams.censusLog = probe.start

	d.ctx, d.cancel = context.WithCancel(context.Background())
	t.Cleanup(d.cancel)
	done := make(chan error, 1)
	go func() { done <- d.Run() }()

	waitForSignal(t, probe.started, "Run never started the census: the start path does not wire it in")
	if len(intervals) != 1 || intervals[0] != time.Minute {
		t.Fatalf("Run asked for ticker intervals %v, want one of %v", intervals, time.Minute)
	}

	first := time.Date(2026, time.October, 26, 11, 30, 0, 0, time.UTC)
	sendCensusTick(t, ticks, first)
	sink.waitForLine(t, "census tick at 2026-10-26T11:30:00Z")

	sendCensusTick(t, ticks, first.Add(time.Minute))
	sink.waitForLine(t, "census tick at 2026-10-26T11:31:00Z")
	if lines := sink.countContaining("census tick at"); lines != 2 {
		t.Errorf("logged %d census lines for 2 ticks, want one per tick", lines)
	}

	d.cancel()
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after the daemon context ended = %v, want a clean shutdown", err)
		}
	case <-bounded.Done():
		t.Fatal("Run did not return after the daemon context ended")
	}

	waitForSignal(t, probe.stopped, "the census kept running after the daemon context ended")
	if stops != 1 {
		t.Errorf("ticker stopped %d times on Run's exit, want once", stops)
	}
}
