package procutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The tests here drive Stoppable with scripted runs and no processes: the
// process-table proof that a restart leaves no child behind is in the
// integration tier (stoppable_integration_test.go). The bounds are values the
// code waits on, so the cases that must not time out give them a day and the
// ones that must time out give them a millisecond; no assertion reads a clock.
const never = 24 * time.Hour

// logs collects what Quiesce says.
type logs struct{ lines []string }

func (l *logs) logf(format string, args ...any) { l.lines = append(l.lines, fmt.Sprintf(format, args...)) }

func (l *logs) text() string { return strings.Join(l.lines, "\n") }

// A poll in flight is waited for: Quiesce returns only after the run has, and
// kills nothing.
func TestQuiesceWaitsForTheRunToReturnAndKillsNothing(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	finish := make(chan struct{})
	returned := make(chan struct{})
	s := StartStoppable(context.Background(), func(ctx context.Context) {
		defer close(returned)
		close(started)
		<-ctx.Done() // the hub stops starting polls...
		<-finish     // ...and the one in flight takes its time
	})
	<-started

	killed := false
	var l logs
	go close(finish)
	s.Quiesce(QuiesceBounds{Drain: never, Kill: never}, func() int { killed = true; return 0 }, l.logf)

	select {
	case <-returned:
	default:
		t.Fatal("Quiesce returned while the run was still in flight")
	}
	if killed {
		t.Fatal("killed children of a poll that was about to finish on its own")
	}
	if len(l.lines) != 0 {
		t.Fatalf("logged %q for a clean drain", l.text())
	}
}

// A run still going after the drain bound has its children killed, and Quiesce
// waits for it again; the kill is what lets it return.
func TestQuiesceKillsStragglersAfterTheDrainBound(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{})
	s := StartStoppable(context.Background(), func(context.Context) { <-stuck })

	var l logs
	s.Quiesce(QuiesceBounds{Drain: time.Millisecond, Kill: never}, func() int {
		close(stuck) // the kill: the child dies, the poll's Wait returns
		return 3
	}, l.logf)

	if want := "killed 3 child process(es)"; !strings.Contains(l.text(), want) {
		t.Fatalf("log = %q, want it to say %q", l.text(), want)
	}
	if strings.Contains(l.text(), "restarting anyway") {
		t.Fatalf("log = %q: said it gave up on a run the kill released", l.text())
	}
}

// A run that outlives both bounds does not hold the restart: Quiesce returns
// and says so.
func TestQuiesceGivesUpOnARunThatNeverReturns(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	s := StartStoppable(context.Background(), func(context.Context) { <-stuck })

	var l logs
	s.Quiesce(QuiesceBounds{Drain: time.Millisecond, Kill: time.Millisecond}, func() int { return 0 }, l.logf)

	if want := "restarting anyway"; !strings.Contains(l.text(), want) {
		t.Fatalf("log = %q, want it to say %q", l.text(), want)
	}
}

// resume starts the run again for a restart whose exec failed, and not at all
// once the process is stopping.
func TestQuiesceResumeRestartsTheRunUnlessTheParentEnded(t *testing.T) {
	t.Parallel()
	starts := make(chan context.Context, 3)
	run := func(ctx context.Context) {
		starts <- ctx
		<-ctx.Done()
	}
	bounds := QuiesceBounds{Drain: never, Kill: never}
	kill := func() int { return 0 }
	var l logs

	parent, stop := context.WithCancel(context.Background())
	defer stop()
	s := StartStoppable(parent, run)
	first := <-starts

	resume := s.Quiesce(bounds, kill, l.logf)
	if first.Err() == nil {
		t.Fatal("Quiesce left the first run going")
	}
	resume()
	second := <-starts
	if second.Err() != nil {
		t.Fatal("resume started a run that is already stopped")
	}

	resume = s.Quiesce(bounds, kill, l.logf)
	stop()
	resume()
	select {
	case <-starts:
		t.Fatal("resume started the run after its parent ended")
	default:
	}
}
