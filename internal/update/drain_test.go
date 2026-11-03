package update

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

const (
	drainTestTimeout = 30 * time.Second
	drainTestIdle    = 5 * time.Minute
	drainTestPoll    = 15 * time.Second
)

// fakeDrainClock is a Clock whose sleep advances Now, so a drain finishes at
// once and the sleeps it made are on record. onSleep, when set, runs after the
// clock advances, so a test can cancel the drain during a sleep.
type fakeDrainClock struct {
	now     time.Time
	sleeps  []time.Duration
	onSleep func()
}

func (c *fakeDrainClock) Now() time.Time { return c.now }

// sleep returns the context's error at once when ctx is done, as the real
// sleep does, and otherwise advances the clock by d and records the wait.
func (c *fakeDrainClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep()
	}
	return ctx.Err()
}

// fakeDrainProbe answers Seats from a function of the call number. It has no
// other method, so a drain that tried to act on a seat could not compile
// against it.
type fakeDrainProbe struct {
	answer func(n int) ([]Seat, error)
	calls  int
}

func (p *fakeDrainProbe) Seats(context.Context) ([]Seat, error) {
	p.calls++
	return p.answer(p.calls)
}

func drainTestStart() time.Time {
	return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
}

func drainTestOptions() DrainOptions {
	return DrainOptions{Timeout: drainTestTimeout, IdleAfter: drainTestIdle, Poll: drainTestPoll}
}

func TestDrainReturnsWhenSeatsFinish(t *testing.T) {
	t.Parallel()
	clock := &fakeDrainClock{now: drainTestStart()}
	probe := &fakeDrainProbe{answer: func(n int) ([]Seat, error) {
		if n < 3 {
			return []Seat{{Name: "furiosa", Working: true, LastActive: clock.Now()}}, nil
		}
		return []Seat{{Name: "furiosa", Working: false}}, nil
	}}

	res, err := Drain(context.Background(), clock, clock.sleep, probe, DrainOptions{
		Timeout: time.Minute, IdleAfter: drainTestIdle, Poll: drainTestPoll,
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !res.Drained {
		t.Errorf("Drained = false, want true: %+v", res)
	}
	if len(clock.sleeps) != 2 {
		t.Errorf("sleeps = %d, want 2", len(clock.sleeps))
	}
	if probe.calls != 3 {
		t.Errorf("Seats calls = %d, want 3", probe.calls)
	}
	if res.Waited != 30*time.Second {
		t.Errorf("Waited = %v, want 30s", res.Waited)
	}
}

func TestDrainTreatsASilentSeatAsDrained(t *testing.T) {
	t.Parallel()
	t.Run("a working seat silent for IdleAfter does not block", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
			return []Seat{{Name: "toast", Working: true, LastActive: clock.Now().Add(-6 * time.Minute)}}, nil
		}}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if !res.Drained {
			t.Errorf("Drained = false, want true: %+v", res)
		}
		if len(clock.sleeps) != 0 {
			t.Errorf("sleeps = %d, want 0", len(clock.sleeps))
		}
	})

	t.Run("a working seat silent for exactly IdleAfter does not block", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
			return []Seat{{Name: "toast", Working: true, LastActive: clock.Now().Add(-drainTestIdle)}}, nil
		}}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if !res.Drained {
			t.Errorf("Drained = false, want true: %+v", res)
		}
	})

	t.Run("a seat that is not working does not block", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
			return []Seat{{Name: "toast", Working: false, LastActive: clock.Now()}}, nil
		}}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if !res.Drained {
			t.Errorf("Drained = false, want true: %+v", res)
		}
	})

	t.Run("an empty seat list is drained at once", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) { return nil, nil }}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if !res.Drained {
			t.Errorf("Drained = false, want true: %+v", res)
		}
		if probe.calls != 1 {
			t.Errorf("Seats calls = %d, want 1", probe.calls)
		}
		if len(clock.sleeps) != 0 {
			t.Errorf("sleeps = %d, want 0", len(clock.sleeps))
		}
	})
}

func TestDrainTimesOutNamingTheBlockers(t *testing.T) {
	t.Parallel()
	clock := &fakeDrainClock{now: drainTestStart()}
	probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
		// The seat reports itself active on every poll, so the idle rule never
		// drains it.
		return []Seat{{Name: "furiosa", Working: true, LastActive: clock.Now()}}, nil
	}}

	res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Drained {
		t.Errorf("Drained = true at the timeout, want false: %+v", res)
	}
	if want := []string{"furiosa"}; !reflect.DeepEqual(res.Blocking, want) {
		t.Errorf("Blocking = %v, want %v", res.Blocking, want)
	}
	if res.Waited < drainTestTimeout {
		t.Errorf("Waited = %v, want at least %v", res.Waited, drainTestTimeout)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil", res.Err)
	}
}

func TestDrainProbeErrorIsNotDrained(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("heartbeat store unreadable")

	t.Run("a probe that always errors ends the drain with the error", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) { return nil, probeErr }}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if res.Drained {
			t.Errorf("Drained = true with an unreadable seat list, want false: %+v", res)
		}
		if !errors.Is(res.Err, probeErr) {
			t.Errorf("Err = %v, want %v", res.Err, probeErr)
		}
		if res.Waited < drainTestTimeout {
			t.Errorf("Waited = %v, want at least %v", res.Waited, drainTestTimeout)
		}
		if probe.calls != 3 {
			t.Errorf("Seats calls = %d, want 3 (a retry on each poll)", probe.calls)
		}
	})

	t.Run("a probe that errors once is retried", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(n int) ([]Seat, error) {
			if n == 1 {
				return nil, probeErr
			}
			return []Seat{{Name: "furiosa", Working: false}}, nil
		}}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if !res.Drained {
			t.Errorf("Drained = false after the retry found no blocker, want true: %+v", res)
		}
		if res.Err != nil {
			t.Errorf("Err = %v, want nil", res.Err)
		}
		if probe.calls != 2 {
			t.Errorf("Seats calls = %d, want 2", probe.calls)
		}
		if len(clock.sleeps) != 1 {
			t.Errorf("sleeps = %d, want 1", len(clock.sleeps))
		}
	})
}

func TestDrainNeverActsOnSeats(t *testing.T) {
	t.Parallel()

	t.Run("the probe is read only and only Seats is called", func(t *testing.T) {
		t.Parallel()
		clock := &fakeDrainClock{now: drainTestStart()}
		seats := []Seat{{Name: "furiosa", Working: true}}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
			seats[0].LastActive = clock.Now() // the seat keeps reporting activity
			return seats, nil
		}}

		typ := reflect.TypeOf(probe)
		if typ.NumMethod() != 1 || typ.Method(0).Name != "Seats" {
			t.Fatalf("the probe exposes %d methods; a drain must have no seat action to call", typ.NumMethod())
		}

		res, err := Drain(context.Background(), clock, clock.sleep, probe, drainTestOptions())
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if res.Drained {
			t.Errorf("Drained = true, want false: %+v", res)
		}
		if probe.calls != 3 {
			t.Errorf("Seats calls = %d, want 3 and no other call", probe.calls)
		}
		if got := seats[0]; !got.Working || got.Name != "furiosa" {
			t.Errorf("the drain changed the seat: %+v", got)
		}
	})

	t.Run("cancelling the context during a sleep returns the context error", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeDrainClock{now: drainTestStart(), onSleep: cancel}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
			return []Seat{{Name: "furiosa", Working: true, LastActive: clock.Now()}}, nil
		}}

		res, err := Drain(ctx, clock, clock.sleep, probe, DrainOptions{
			Timeout: time.Minute, IdleAfter: drainTestIdle, Poll: drainTestPoll,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain error = %v, want context.Canceled", err)
		}
		if res.Drained {
			t.Errorf("Drained = true after cancellation, want false: %+v", res)
		}
		if probe.calls != 1 {
			t.Errorf("Seats calls = %d, want 1 (no poll after cancellation)", probe.calls)
		}
	})

	t.Run("an already cancelled context never probes", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		clock := &fakeDrainClock{now: drainTestStart()}
		probe := &fakeDrainProbe{answer: func(int) ([]Seat, error) {
			return nil, errors.New("probe must not be called")
		}}

		if _, err := Drain(ctx, clock, clock.sleep, probe, drainTestOptions()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain error = %v, want context.Canceled", err)
		}
		if probe.calls != 0 {
			t.Errorf("Seats calls = %d, want 0", probe.calls)
		}
		if len(clock.sleeps) != 0 {
			t.Errorf("sleeps = %d, want 0", len(clock.sleeps))
		}
	})
}
