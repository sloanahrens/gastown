package runnerroll

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock advances only when Sleep is called, so the poll loop never waits.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

// reading is one scripted health reading.
type reading struct {
	pause  string
	passes int
	known  bool
	err    error
}

// scripted returns the readings in order and repeats the last one after that.
func scripted(rs ...reading) (func(context.Context) (string, int, bool, error), *int) {
	calls := 0
	return func(context.Context) (string, int, bool, error) {
		i := min(calls, len(rs)-1)
		calls++
		r := rs[i]
		return r.pause, r.passes, r.known, r.err
	}, &calls
}

func TestWaitLandingIdleReturnsWhenRollPauseClears(t *testing.T) {
	read, _ := scripted(
		reading{pause: "", passes: 2, known: true},
		reading{pause: "someone else", passes: 0, known: true},
		reading{pause: RollPause, passes: 1, known: true},
		reading{pause: RollPause, passes: 0, known: true},
	)
	clock := &fakeClock{}
	var out bytes.Buffer
	if err := WaitLandingIdle(context.Background(), read, clock, time.Minute, &out); err != nil {
		t.Fatalf("WaitLandingIdle() = %v, want nil", err)
	}
	if len(clock.sleeps) != 3 {
		t.Errorf("slept %d times, want 3 (one per non-idle reading)", len(clock.sleeps))
	}
}

func TestWaitLandingIdlePrintsEachChangeOnce(t *testing.T) {
	read, _ := scripted(
		reading{pause: RollPause, passes: 2, known: true},
		reading{pause: RollPause, passes: 2, known: true},
		reading{pause: RollPause, passes: 1, known: true},
		reading{pause: RollPause, passes: 1, known: true},
		reading{pause: RollPause, passes: 0, known: true},
	)
	var out bytes.Buffer
	if err := WaitLandingIdle(context.Background(), read, &fakeClock{}, time.Minute, &out); err != nil {
		t.Fatalf("WaitLandingIdle() = %v, want nil", err)
	}
	want := `landing: pause "runner roll", 2 passes in flight
landing: pause "runner roll", 1 passes in flight
landing: pause "runner roll", 0 passes in flight
`
	if got := out.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestWaitLandingTimeoutNamesPasses(t *testing.T) {
	read, _ := scripted(reading{pause: RollPause, passes: 3, known: true})
	clock := &fakeClock{}
	start := clock.Now()
	var out bytes.Buffer
	err := WaitLandingIdle(context.Background(), read, clock, 12*time.Second, &out)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("WaitLandingIdle() = %v, want ErrWaitTimeout", err)
	}
	if !strings.Contains(err.Error(), "3 landing passes still in flight") {
		t.Errorf("error %q does not name the passes count", err)
	}
	if got := clock.Now().Sub(start); got != 12*time.Second {
		t.Errorf("waited until %v, want exactly the 12s bound", got)
	}
}

func TestWaitLandingUnknownPassesIsError(t *testing.T) {
	// The roll's own pause with zero passes would read as idle if the unknown
	// flag were ignored, so the reading below must be refused, not accepted.
	read, calls := scripted(reading{pause: RollPause, passes: 0, known: false})
	err := WaitLandingIdle(context.Background(), read, &fakeClock{}, time.Minute, &bytes.Buffer{})
	if err == nil {
		t.Fatal("WaitLandingIdle() = nil on unknown passes, want an error")
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Errorf("unknown passes reported as a timeout: %v", err)
	}
	if *calls != 1 {
		t.Errorf("read %d times, want 1 (the wait stops at the unknown reading)", *calls)
	}
}

func TestWaitLandingReadErrorIsReturned(t *testing.T) {
	boom := errors.New("health file torn")
	read, _ := scripted(reading{err: boom})
	err := WaitLandingIdle(context.Background(), read, &fakeClock{}, time.Minute, &bytes.Buffer{})
	if !errors.Is(err, boom) {
		t.Fatalf("WaitLandingIdle() = %v, want the read error wrapped", err)
	}
}

func TestWaitLandingCancelDuringReadIsNotIdle(t *testing.T) {
	// The read is canceled after it returns an idle reading: that reading was
	// taken while the wait was being stopped, so it must not decide the wait.
	ctx, cancel := context.WithCancel(context.Background())
	read := func(context.Context) (string, int, bool, error) {
		cancel()
		return RollPause, 0, true, nil
	}
	err := WaitLandingIdle(ctx, read, &fakeClock{}, time.Minute, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitLandingIdle() = %v, want context.Canceled, not idle", err)
	}
}

func TestWaitLandingCanceledContextStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read, calls := scripted(reading{pause: RollPause, passes: 0, known: true})
	err := WaitLandingIdle(ctx, read, &fakeClock{}, time.Minute, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitLandingIdle() = %v, want context.Canceled", err)
	}
	if *calls != 0 {
		t.Errorf("read %d times on a canceled context, want 0", *calls)
	}
}
