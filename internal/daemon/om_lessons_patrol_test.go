package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// omLessonsTestDaemon builds a daemon whose om_lessons patrol is enabled.
func omLessonsTestDaemon(t *testing.T, logged io.Writer) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OmLessons: &agentconfig.OmLessonsConfig{Enabled: true},
		}},
	}
}

// TestReworkProgramOmLessonsPatrolStartsOnlyWhenEnabled: the patrol's own
// ticker exists only when daemon.patrols.om_lessons.enabled is true, and it is
// off by default. A configured interval it cannot schedule starts no ticker and
// names the key once.
func TestReworkProgramOmLessonsPatrolStartsOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	var intervals []time.Duration
	ticks := make(chan time.Time)
	stops := 0
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	d.newOmLessonsTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	if channel, stop := d.startOmLessonsTicker(); channel != nil || stop != nil {
		t.Error("a town that names no om_lessons started its ticker")
	}
	if len(intervals) != 0 {
		t.Fatalf("a town that names no om_lessons asked for tickers at %v, want none", intervals)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: false},
	}}
	if channel, stop := d.startOmLessonsTicker(); channel != nil || stop != nil {
		t.Error("a disabled om_lessons started its ticker")
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: true},
	}}
	channel, stop := d.startOmLessonsTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled om_lessons started no ticker")
	}
	if len(intervals) != 1 || intervals[0] != 168*time.Hour {
		t.Errorf("the ticker asks for an interval of %v, want the 168h default", intervals)
	}
	stop()
	if stops != 1 {
		t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: true, Interval: "48h"},
	}}
	channel, stop = d.startOmLessonsTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled om_lessons with an interval started no ticker")
	}
	if len(intervals) != 2 || intervals[1] != 48*time.Hour {
		t.Errorf("the ticker asks for an interval of %v, want the configured 48h", intervals)
	}
	stop()

	// An interval under the floor starts no ticker rather than one the town did
	// not ask for, and names the key once.
	var logged bytes.Buffer
	d.logger = log.New(&logged, "", 0)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: true, Interval: "30m"},
	}}
	if channel, stop := d.startOmLessonsTicker(); channel != nil || stop != nil {
		t.Error("an interval under the floor started a ticker")
	}
	if len(intervals) != 2 {
		t.Errorf("an interval under the floor asked for a ticker at %v, want none", intervals[2:])
	}
	if !strings.Contains(logged.String(), "patrols.om_lessons.interval") {
		t.Errorf("an interval under the floor logged %q, want the interval key named", logged.String())
	}

	// The production ticker, with no seam: the patrol still gets a channel and
	// a stop func, so Run's defer has a real ticker to stop.
	plain := omLessonsTestDaemon(t, io.Discard)
	channel, stop = plain.startOmLessonsTicker()
	if channel == nil || stop == nil {
		t.Fatal("the patrol started no ticker without a ticker seam")
	}
	stop()
}

// TestReworkProgramOmLessonsEachTickCallsTheRunSeamOnce: one started tick calls
// the run seam once, and the pass it built is what the tick reports.
func TestReworkProgramOmLessonsEachTickCallsTheRunSeamOnce(t *testing.T) {
	t.Parallel()

	t.Run("one tick, one call", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)
		var calls atomic.Int32
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			calls.Add(1)
			return []omlessons.Proposal{{ID: "fail-open"}, {ID: "missing-refusal-test"}}, nil
		}

		d.startOmLessonsTick()
		d.omLessonsCycles.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("a tick called the run seam %d times, want once", got)
		}
		if d.omLessonsRunning.Load() {
			t.Error("the guard stayed set after the pass ended, so no later tick could run")
		}
		if !strings.Contains(logged.String(), "the pass built 2 proposal(s)") {
			t.Errorf("the tick logged %q, want the two proposals it built", logged.String())
		}
	})

	t.Run("a tick while one runs is skipped", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var calls atomic.Int32
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			calls.Add(1)
			reached <- struct{}{}
			<-release
			return nil, nil
		}

		d.startOmLessonsTick()
		waitForSignal(t, reached, "the first pass never reached the run seam")
		d.startOmLessonsTick()
		if !strings.Contains(logged.String(), "previous pass still running, skipping") {
			t.Errorf("the second tick was not logged as skipped: %q", logged.String())
		}
		close(release)
		d.omLessonsCycles.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("two starting ticks called the run seam %d times, want once", got)
		}
	})
}

// TestReworkProgramOmLessonsSkeletonPassProposesNothing: a daemon with no run
// seam wired runs the skeleton, which reads nothing and proposes nothing, and
// a pass that fails is logged rather than counted as a clean one.
func TestReworkProgramOmLessonsSkeletonPassProposesNothing(t *testing.T) {
	t.Parallel()

	t.Run("the skeleton proposes nothing", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)

		d.triggerOmLessons()

		if !strings.Contains(logged.String(), "the pass built 0 proposal(s)") {
			t.Errorf("the skeleton pass logged %q, want the zero proposals it built", logged.String())
		}
	})

	t.Run("a failed pass is logged, not counted", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			return nil, errors.New("the review records are unreadable")
		}

		d.triggerOmLessons()

		if !strings.Contains(logged.String(), "the review records are unreadable") {
			t.Errorf("a failed pass logged %q, want the error named", logged.String())
		}
		if strings.Contains(logged.String(), "proposal(s)") {
			t.Errorf("a failed pass logged a proposal count: %q", logged.String())
		}
	})
}

// TestReworkProgramOmLessonsStopJoinsTheRunningPass: Run's return stops the
// ticker and waits for the pass it started, and a pass still running at the
// bound is logged and left.
func TestReworkProgramOmLessonsStopJoinsTheRunningPass(t *testing.T) {
	t.Parallel()

	blocking := func(t *testing.T, logged io.Writer) (*Daemon, chan struct{}, func()) {
		t.Helper()
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		d := omLessonsTestDaemon(t, logged)
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			reached <- struct{}{}
			<-release
			return nil, nil
		}
		t.Cleanup(func() {
			unblock()
			d.omLessonsCycles.Wait()
		})
		return d, reached, unblock
	}

	t.Run("the pass the ticker started is joined", func(t *testing.T) {
		t.Parallel()
		d, reached, unblock := blocking(t, io.Discard)
		d.omLessonsJoinWait = time.Hour

		d.startOmLessonsTick()
		waitForSignal(t, reached, "the pass never reached the run seam")

		stopped := make(chan struct{})
		stopCalled := make(chan struct{})
		go func() {
			d.stopOmLessonsPatrol(func() { close(stopCalled) })
			close(stopped)
		}()
		waitForSignal(t, stopCalled, "the stop func never ran")
		select {
		case <-stopped:
			t.Fatal("stopOmLessonsPatrol returned while the pass still ran")
		default:
		}
		unblock()
		waitForSignal(t, stopped, "stopOmLessonsPatrol never returned after the pass finished")
		if d.omLessonsRunning.Load() {
			t.Error("the join returned with the pass's guard still set")
		}
	})

	t.Run("a pass past the bound is logged and left", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d, reached, unblock := blocking(t, &logged)
		d.omLessonsJoinWait = 30 * time.Millisecond

		d.startOmLessonsTick()
		waitForSignal(t, reached, "the pass never reached the run seam")

		stopped := make(chan struct{})
		go func() {
			d.stopOmLessonsPatrol(nil)
			close(stopped)
		}()
		waitForSignal(t, stopped, "stopOmLessonsPatrol never returned past the bound")
		if !strings.Contains(logged.String(), "still running") {
			t.Errorf("a pass left past the bound was not logged: %q", logged.String())
		}
		unblock()
		d.omLessonsCycles.Wait()
	})
}
