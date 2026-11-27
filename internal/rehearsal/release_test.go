package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

var releaseEpoch = time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)

func TestRehearsalReleaseOutcomesAndSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		outcome string
	}{
		{"nil is ok", nil, SlingOK},
		{"a plain error is failed", errors.New("boom"), SlingFailed},
		{"a wrapped deadline is killed", fmt.Errorf("sling: %w", context.DeadlineExceeded), SlingKilled},
		{"deadline text without the wrap is failed", errors.New("context deadline exceeded"), SlingFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := clockwork.NewFakeClockAt(releaseEpoch)
			sling := func(context.Context, string) error { clk.Advance(2 * time.Second); return tc.err }
			got := Release(t.Context(), []string{"bead-1"}, 1, sling, clk.Now)
			want := []Sling{{Bead: "bead-1", Outcome: tc.outcome, Seconds: 2}}
			if !slices.Equal(got, want) {
				t.Errorf("Release = %+v, want %+v", got, want)
			}
		})
	}
}

// boundedCtx gives a test a deadline so a wait that never ends fails the test instead of hanging the gate.
func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRehearsalReleaseNeverExceedsInFlight(t *testing.T) {
	t.Parallel()
	ctx := boundedCtx(t)
	const inFlight = 3
	ids := []string{"b01", "b02", "b03", "b04", "b05", "b06", "b07", "b08", "b09", "b10", "b11", "b12"}
	var running, peak atomic.Int32
	var once sync.Once
	full, unblock := make(chan struct{}), make(chan struct{})
	sling := func(ctx context.Context, _ string) error {
		n := running.Add(1)
		defer running.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if n == inFlight {
			once.Do(func() { close(full) })
		}
		select {
		case <-unblock:
		case <-ctx.Done():
		}
		return nil
	}
	clk := clockwork.NewFakeClockAt(releaseEpoch)
	done := make(chan []Sling, 1)
	go func() { done <- Release(ctx, ids, inFlight, sling, clk.Now) }()
	select {
	case <-full:
	case <-ctx.Done():
		t.Fatalf("fewer than %d slings ran at once before the deadline: %v", inFlight, ctx.Err())
	}
	close(unblock)
	var got []Sling
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatalf("Release did not return before the deadline: %v", ctx.Err())
	}
	if p := peak.Load(); p != inFlight {
		t.Errorf("peak concurrent slings = %d, want %d", p, inFlight)
	}
	for i, s := range got {
		if s.Bead != ids[i] || s.Outcome != SlingOK {
			t.Errorf("result %d = %+v, want bead %q ok", i, s, ids[i])
		}
	}
}

func TestRehearsalReleaseStartsInIdOrder(t *testing.T) {
	t.Parallel()
	// An inFlight of 0 is below one and must run one at a time, not deadlock.
	for _, inFlight := range []int{1, 0} {
		t.Run(fmt.Sprintf("inFlight %d", inFlight), func(t *testing.T) {
			t.Parallel()
			ids := []string{"b1", "b2", "b3", "b4"}
			var mu sync.Mutex
			var started []string
			sling := func(_ context.Context, id string) error {
				mu.Lock()
				defer mu.Unlock()
				started = append(started, id)
				return nil
			}
			Release(t.Context(), ids, inFlight, sling, clockwork.NewFakeClockAt(releaseEpoch).Now)
			if !slices.Equal(started, ids) {
				t.Errorf("started %v, want %v", started, ids)
			}
		})
	}
}

func TestRehearsalReleaseReportsInIdOrderWhenSlingsFinishOutOfOrder(t *testing.T) {
	t.Parallel()
	ctx := boundedCtx(t)
	lastDone := make(chan struct{})
	sling := func(ctx context.Context, id string) error {
		switch id {
		case "b1":
			select {
			case <-lastDone:
			case <-ctx.Done():
			}
		case "b4":
			defer close(lastDone)
			return errors.New("boom")
		}
		return nil
	}
	got := Release(ctx, []string{"b1", "b2", "b3", "b4"}, 4, sling, clockwork.NewFakeClockAt(releaseEpoch).Now)
	if got[0] != (Sling{Bead: "b1", Outcome: SlingOK}) || got[3] != (Sling{Bead: "b4", Outcome: SlingFailed}) {
		t.Errorf("Release = %+v, want b1 ok first and b4 failed last", got)
	}
}

func TestRehearsalReleaseStopsStartingAfterCancel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cancelFirst bool
		wantStarted []string
	}{
		{"canceled before the call starts no sling", true, nil},
		{"cancel while the first sling runs starts no later one", false, []string{"b1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deadline := boundedCtx(t)
			ctx, cancel := context.WithCancel(deadline)
			t.Cleanup(cancel)
			ids := []string{"b1", "b2", "b3", "b4"}
			var mu sync.Mutex
			var started []string
			firstStarted := make(chan struct{})
			var once sync.Once
			sling := func(ctx context.Context, id string) error {
				mu.Lock()
				started = append(started, id)
				mu.Unlock()
				once.Do(func() { close(firstStarted) })
				select {
				case <-ctx.Done():
				case <-deadline.Done():
				}
				return ctx.Err()
			}
			if tc.cancelFirst {
				cancel()
			}
			done := make(chan []Sling, 1)
			go func() { done <- Release(ctx, ids, 1, sling, clockwork.NewFakeClockAt(releaseEpoch).Now) }()
			if !tc.cancelFirst {
				select {
				case <-firstStarted:
				case <-deadline.Done():
					t.Fatalf("no sling started before the deadline: %v", deadline.Err())
				}
				cancel()
			}
			var got []Sling
			select {
			case got = <-done:
			case <-deadline.Done():
				t.Fatalf("Release did not return before the deadline: %v", deadline.Err())
			}
			mu.Lock()
			gotStarted := started
			mu.Unlock()
			if !slices.Equal(gotStarted, tc.wantStarted) {
				t.Errorf("started %v, want %v", gotStarted, tc.wantStarted)
			}
			want := make([]Sling, len(ids))
			for i, id := range ids {
				want[i] = Sling{Bead: id, Outcome: SlingFailed}
			}
			if !slices.Equal(got, want) {
				t.Errorf("Release = %+v, want %+v", got, want)
			}
		})
	}
}
