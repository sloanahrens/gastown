package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
)

// clusterDriftStartedAt is a drift daemon whose clock is at start and whose
// start is recorded as New records it (gt-myyr6e).
func clusterDriftStartedAt(t *testing.T, start time.Time) (*Daemon, *clockwork.FakeClock) {
	t.Helper()
	d, fc := clusterDriftDaemon(t, start)
	d.clusterDriftCache.started = start
	return d, fc
}

// clusterDriftAtOffset moves the clock to start plus offset and runs one
// heartbeat over the sources the daemon builds there, with prev as the queue
// the heartbeat reads. Offsets must not go back in time.
func clusterDriftAtOffset(t *testing.T, d *Daemon, fc *clockwork.FakeClock, start time.Time, offset time.Duration, prev attention.State) []attention.Item {
	t.Helper()
	fc.Advance(start.Add(offset).Sub(fc.Now()))
	s := d.attentionSources(fc.Now())
	s.prev = prev
	return collectClusterDriftOnce(t, s)
}

// TestClusterDriftPatrolWaitsThreeMinutesAfterStart is the first box: inside
// the start delay the heartbeats call no compare and raise nothing, though a
// difference is waiting, and the heartbeat at three minutes makes the first
// run and raises the difference (gt-myyr6e).
func TestClusterDriftPatrolWaitsThreeMinutesAfterStart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	d, fc := clusterDriftStartedAt(t, start)
	var callAt []time.Duration
	d.clusterDriftCompare = func(context.Context) (clusterDriftResult, error) {
		callAt = append(callAt, fc.Now().Sub(start))
		return clusterDriftResult{Differing: []string{"Deployment/gastown-daemon: spec.replicas 2 -> 3"}}, nil
	}

	for _, offset := range []time.Duration{0, time.Minute, 3*time.Minute - time.Nanosecond} {
		if items := clusterDriftAtOffset(t, d, fc, start, offset, attention.State{}); len(items) != 0 {
			t.Errorf("at %v items = %+v, want nothing inside the start delay", offset, items)
		}
	}
	if len(callAt) != 0 {
		t.Fatalf("compare ran at %v inside the start delay, want no call", callAt)
	}

	items := clusterDriftAtOffset(t, d, fc, start, 3*time.Minute, attention.State{})
	assertClusterDriftItem(t, items, attention.RuleClusterDrift, attention.KindClusterDrift)
	if len(callAt) != 1 || callAt[0] != 3*time.Minute {
		t.Errorf("compare calls at %v, want one call at 3m0s", callAt)
	}
}

// TestClusterDriftPatrolFailureAfterTheDelayRaisesAtOnce is the second box: a
// read that fails at four minutes raises cluster.drift_unreadable on that
// heartbeat, and the next compare call is at 19 minutes, the 15 minute cadence
// from the failed run, not sooner (gt-myyr6e).
func TestClusterDriftPatrolFailureAfterTheDelayRaisesAtOnce(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	d, fc := clusterDriftStartedAt(t, start)
	var callAt []time.Duration
	d.clusterDriftCompare = func(context.Context) (clusterDriftResult, error) {
		callAt = append(callAt, fc.Now().Sub(start))
		return clusterDriftResult{}, errors.New("the cluster read was refused")
	}

	items := clusterDriftAtOffset(t, d, fc, start, 4*time.Minute, attention.State{})
	assertClusterDriftItem(t, items, attention.RuleClusterDriftUnreadable, attention.KindClusterDriftUnreadable)

	items = clusterDriftAtOffset(t, d, fc, start, 19*time.Minute-time.Nanosecond, attention.State{})
	assertClusterDriftItem(t, items, attention.RuleClusterDriftUnreadable, attention.KindClusterDriftUnreadable)
	if len(callAt) != 1 {
		t.Fatalf("compare calls at %v before 19m, want only the 4m run", callAt)
	}

	items = clusterDriftAtOffset(t, d, fc, start, 19*time.Minute, attention.State{})
	assertClusterDriftItem(t, items, attention.RuleClusterDriftUnreadable, attention.KindClusterDriftUnreadable)
	want := []time.Duration{4 * time.Minute, 19 * time.Minute}
	if len(callAt) != len(want) || callAt[0] != want[0] || callAt[1] != want[1] {
		t.Errorf("compare calls at %v, want %v", callAt, want)
	}
}

// TestClusterDriftPatrolHoldsThePreviousItemsDuringTheDelay is the third box:
// inside the start delay the heartbeat returns the previous drift and
// unreadable items unchanged and calls no compare, so a roll neither raises
// nor clears anything (gt-myyr6e).
func TestClusterDriftPatrolHoldsThePreviousItemsDuringTheDelay(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	d, fc := clusterDriftStartedAt(t, start)
	calls := 0
	d.clusterDriftCompare = func(context.Context) (clusterDriftResult, error) {
		calls++
		return clusterDriftResult{}, nil
	}
	drift := clusterDriftItems(clusterDriftResult{Differing: []string{"Deployment/gastown-daemon: spec.replicas 2 -> 3"}}, nil)
	unreadable := clusterDriftItems(clusterDriftResult{}, errors.New("the cluster read was refused"))
	prev := attention.State{Items: append(append([]attention.Item{}, drift...), unreadable...)}

	items := clusterDriftAtOffset(t, d, fc, start, time.Minute, prev)
	if calls != 0 {
		t.Errorf("the compare was called %d times inside the start delay, want none", calls)
	}
	if len(items) != len(prev.Items) {
		t.Fatalf("items = %+v, want the previous items %+v", items, prev.Items)
	}
	for i := range prev.Items {
		if items[i].Key != prev.Items[i].Key || items[i].Kind != prev.Items[i].Kind || items[i].Summary != prev.Items[i].Summary {
			t.Errorf("item %d = %+v, want the previous item %+v unchanged", i, items[i], prev.Items[i])
		}
	}
}

// TestClusterDriftPatrolWaitIsOffWithoutAFunc: a sources value that carries no
// start check waits for nothing, so its first heartbeat calls the compare
// (gt-myyr6e).
func TestClusterDriftPatrolWaitIsOffWithoutAFunc(t *testing.T) {
	t.Parallel()
	calls := 0
	s := &attentionSources{
		clusterDriftEnabled: func() bool { return true },
		clusterDriftAnswer: func(context.Context) (clusterDriftResult, error) {
			calls++
			return clusterDriftResult{}, nil
		},
	}
	collectClusterDriftOnce(t, s)
	if calls != 1 {
		t.Errorf("the compare was called %d times with no start check, want 1", calls)
	}
}
