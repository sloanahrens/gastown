package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// outboxPollTestHangGuard bounds how long a test waits for a call that must
// return: it fires only when the code under test hangs, never to pace a wait.
const outboxPollTestHangGuard = 5 * time.Second

// outboxPollTestDaemon builds a daemon whose outbox_poll patrol is in the given
// state: on when enabled is true, and a town that names no key otherwise.
func outboxPollTestDaemon(t *testing.T, enabled bool, logged io.Writer) *Daemon {
	t.Helper()
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if enabled {
		cfg.Patrols.OutboxPoll = &agentconfig.OutboxPollConfig{Enabled: true}
	}
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(logged, "", 0),
		patrolConfig: cfg,
	}
}

// TestNoBeadAccessOutboxPollDefaultsOff pins the key's default: a town that
// does not name outbox_poll, or names it with enabled false, runs no poll, and
// only enabled:true turns it on (gt-0hkj0c).
func TestNoBeadAccessOutboxPollDefaultsOff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config *DaemonPatrolConfig
		want   bool
	}{
		{"no config at all", nil, false},
		{"a config with no patrols", &DaemonPatrolConfig{}, false},
		{"a patrols section that names no outbox_poll", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, false},
		{"outbox_poll with enabled false", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OutboxPoll: &agentconfig.OutboxPollConfig{Enabled: false},
		}}, false},
		{"outbox_poll with enabled true", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OutboxPoll: &agentconfig.OutboxPollConfig{Enabled: true},
		}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPatrolEnabled(tc.config, outboxPollPatrol); got != tc.want {
				t.Errorf("IsPatrolEnabled(outbox_poll) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNoBeadAccessOutboxPollTickerStartsOnlyWhenEnabled: the patrol gets its
// own ticker when the town names outbox_poll, and none when it does not; the
// cadence it asks for is defaultOutboxPollInterval (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickerStartsOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	ticks := make(chan time.Time)
	var intervals []time.Duration
	stops := 0
	d := outboxPollTestDaemon(t, false, io.Discard)
	d.newOutboxPollTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	if channel, stop := d.startOutboxPollTicker(); channel != nil || stop != nil {
		t.Error("a town that does not name outbox_poll started its ticker")
	}
	if len(intervals) != 0 {
		t.Errorf("a town that does not name outbox_poll asked for tickers at %v, want none", intervals)
	}

	on := outboxPollTestDaemon(t, true, io.Discard)
	on.newOutboxPollTicker = d.newOutboxPollTicker
	channel, stop := on.startOutboxPollTicker()
	if channel == nil || stop == nil {
		t.Fatal("a town that names outbox_poll started no ticker")
	}
	if len(intervals) != 1 || intervals[0] != defaultOutboxPollInterval {
		t.Errorf("the ticker asks for an interval of %v, want the %v default", intervals, defaultOutboxPollInterval)
	}
	stop()
	if stops != 1 {
		t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
	}

	// The production ticker, with no seam: the patrol still gets a channel and
	// a stop func, so Run's defer has a real ticker to stop.
	plain := outboxPollTestDaemon(t, true, io.Discard)
	channel, stop = plain.startOutboxPollTicker()
	if channel == nil || stop == nil {
		t.Fatal("the patrol started no ticker without a ticker seam")
	}
	stop()
}

// TestNoBeadAccessOutboxPollTickCallsTheSeamOnce runs one tick and waits on the
// patrol's own cycle count instead of a timer: the seam runs exactly once
// (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickCallsTheSeamOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := outboxPollTestDaemon(t, true, io.Discard)
	d.outboxPollTick = func(context.Context) error {
		calls.Add(1)
		return nil
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the seam ran %d times for one tick, want once", got)
	}
	if d.outboxPollRunning.Load() {
		t.Error("the tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollTickBoundsTheSeam: the seam is handed a context
// with a deadline, so a read that hangs ends at outboxPollTickTimeout rather
// than holding the guard for the daemon's life (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickBoundsTheSeam(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	var hasDeadline bool
	d := outboxPollTestDaemon(t, true, io.Discard)
	d.outboxPollTick = func(ctx context.Context) error {
		deadline, hasDeadline = ctx.Deadline()
		return nil
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if !hasDeadline {
		t.Fatal("the seam's context carries no deadline, so a hung read never ends")
	}
	if left := time.Until(deadline); left <= 0 || left > outboxPollTickTimeout {
		t.Errorf("the seam's deadline is %v away, want at most %v", left, outboxPollTickTimeout)
	}
}

// TestNoBeadAccessOutboxPollTickSkipsAnInactivePatrol: a town that turned the
// patrol off after its ticker started reads nothing, so no seam runs
// (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickSkipsAnInactivePatrol(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := outboxPollTestDaemon(t, false, io.Discard)
	d.outboxPollTick = func(context.Context) error {
		calls.Add(1)
		return nil
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if got := calls.Load(); got != 0 {
		t.Errorf("an inactive patrol ran the seam %d times, want none", got)
	}
}

// TestNoBeadAccessOutboxPollRefusesWhileATickRuns: a tick that arrives while
// the previous one still runs is skipped, so two polls never read the same
// seats at once (gt-0hkj0c).
func TestNoBeadAccessOutboxPollRefusesWhileATickRuns(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)
	d.outboxPollTick = func(context.Context) error {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return nil
	}

	d.startOutboxPollTick()
	<-entered
	d.startOutboxPollTick()
	if !strings.Contains(logged.String(), "previous tick still running, skipping") {
		t.Errorf("the overlapping tick's log is %q, want the refusal named", logged.String())
	}
	close(release)
	d.outboxPollCycles.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the seam ran %d times for two overlapping ticks, want once", got)
	}
}

// TestNoBeadAccessOutboxPollTickWithoutASeamCompletes: the production skeleton
// has no seam yet, and its tick is a no-op that still ends and frees the guard
// (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickWithoutASeamCompletes(t *testing.T) {
	t.Parallel()
	d := outboxPollTestDaemon(t, true, io.Discard)

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if d.outboxPollRunning.Load() {
		t.Error("the no-op tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollLogsASeamError: a seam that fails is logged rather
// than read as a quiet tick, and the guard is released (gt-0hkj0c).
func TestNoBeadAccessOutboxPollLogsASeamError(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)
	d.outboxPollTick = func(context.Context) error {
		return errors.New("the gateway refused the read")
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if !strings.Contains(logged.String(), "outbox_poll: the gateway refused the read") {
		t.Errorf("the tick's log is %q, want the seam's error named", logged.String())
	}
	if d.outboxPollRunning.Load() {
		t.Error("the failed tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollStopLeavesALateTick: Run's stop waits its bound and
// leaves a tick still running, logging that it did, rather than hanging on a
// read that outlasted the daemon (gt-0hkj0c).
func TestNoBeadAccessOutboxPollStopLeavesALateTick(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)
	d.outboxPollJoinWait = time.Millisecond
	d.outboxPollTick = func(context.Context) error {
		entered <- struct{}{}
		<-release
		return nil
	}

	d.startOutboxPollTick()
	<-entered
	stopped := make(chan struct{})
	go func() {
		d.stopOutboxPollPatrol(nil)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(outboxPollTestHangGuard): //testpolicy:allow no-sleep — a hang guard only: the stop's return is the event the test waits for, and a stop that never returns fails here rather than hanging the test
		close(release)
		t.Fatal("the stop waited past its bound for a running tick")
	}
	close(release)
	d.outboxPollCycles.Wait()

	if !strings.Contains(logged.String(), "still running") {
		t.Errorf("the stop's log is %q, want the late tick named", logged.String())
	}
}
