package daemon

import (
	"context"
	"log"
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

// next waits for one more line and returns it.
func (s *censusLogSink) next(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-s.wrote:
	case <-ctx.Done():
		t.Fatal("the daemon logger received no census line")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines[len(s.lines)-1]
}

func (s *censusLogSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines)
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

// TestCensusLogTicksEveryMinuteIntoTheDaemonLogger pins the daemon's census
// wiring (gt-7h3vm): a one-minute ticker whose ticks the census logs, one line
// per tick, through the daemon's own logger, until the daemon's context ends
// and the ticker is stopped.
func TestCensusLogTicksEveryMinuteIntoTheDaemonLogger(t *testing.T) {
	t.Parallel()
	sink := newCensusLogSink()
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{ctx: ctx, logger: log.New(sink, "", 0)}

	ticks := make(chan time.Time)
	var intervals []time.Duration
	stops := 0
	d.seams.censusTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}
	probe := &censusLogProbe{started: make(chan struct{}), stopped: make(chan struct{})}
	d.seams.censusLog = probe.start
	t.Cleanup(func() {
		cancel()
		select {
		case <-probe.started:
		default:
			return // the wiring started nothing, so there is no goroutine to wait for
		}
		ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		select {
		case <-probe.stopped:
		case <-ctx.Done():
			t.Error("the census kept running after the daemon's context ended")
		}
	})

	stop := d.startCensusLog()
	waitForSignal(t, probe.started, "the daemon's wiring never started the census")

	if len(intervals) != 1 || intervals[0] != time.Minute {
		t.Fatalf("ticker intervals = %v, want one of %v", intervals, time.Minute)
	}

	first := time.Date(2026, time.October, 26, 11, 30, 0, 0, time.UTC)
	sendCensusTick(t, ticks, first)
	if line := sink.next(t); !strings.Contains(line, "census tick at 2026-10-26T11:30:00Z") {
		t.Errorf("line = %q, want the tick the ticker delivered", line)
	}

	sendCensusTick(t, ticks, first.Add(time.Minute))
	if line := sink.next(t); !strings.Contains(line, "census tick at 2026-10-26T11:31:00Z") {
		t.Errorf("second line = %q, want the second tick", line)
	}
	if lines := sink.count(); lines != 2 {
		t.Errorf("logged %d lines for 2 ticks, want one per tick", lines)
	}

	cancel()
	waitForSignal(t, probe.stopped, "the census kept running after the daemon's context ended")

	stop()
	if stops != 1 {
		t.Errorf("ticker stopped %d times, want once", stops)
	}
}
