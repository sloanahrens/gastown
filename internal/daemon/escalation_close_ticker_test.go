package daemon

import (
	"testing"
	"time"
)

// fakeEscalationTicker stands in for a real ticker: it records the interval it
// was started with, owns the tick channel, and counts stop calls. Nothing here
// reads a clock.
type fakeEscalationTicker struct {
	interval time.Duration
	ticks    chan time.Time
	stops    int
}

func (f *fakeEscalationTicker) start(interval time.Duration) (<-chan time.Time, func()) {
	f.interval = interval
	return f.ticks, func() { f.stops++ }
}

func TestStartEscalationCloseTickerWith(t *testing.T) {
	t.Parallel()

	t.Run("the interval reaches the ticker seam unchanged", func(t *testing.T) {
		t.Parallel()
		fake := &fakeEscalationTicker{ticks: make(chan time.Time, 1)}
		_, stop := startEscalationCloseTickerWith(90*time.Second, fake.start)
		defer stop()
		if fake.interval != 90*time.Second {
			t.Fatalf("newTicker got interval %v, want %v", fake.interval, 90*time.Second)
		}
	})

	t.Run("the seam's channel comes back", func(t *testing.T) {
		t.Parallel()
		fake := &fakeEscalationTicker{ticks: make(chan time.Time, 1)}
		ticks, stop := startEscalationCloseTickerWith(time.Minute, fake.start)
		defer stop()
		want := time.Unix(1700000000, 0)
		fake.ticks <- want
		select {
		case got := <-ticks:
			if !got.Equal(want) {
				t.Fatalf("received tick %v, want %v", got, want)
			}
		default:
			t.Fatal("a tick sent on the seam's channel was not received from the returned channel")
		}
	})

	t.Run("stop reaches the seam's stop and is safe to call twice", func(t *testing.T) {
		t.Parallel()
		fake := &fakeEscalationTicker{ticks: make(chan time.Time, 1)}
		_, stop := startEscalationCloseTickerWith(time.Minute, fake.start)
		if fake.stops != 0 {
			t.Fatalf("stop was called %d times before the caller stopped, want 0", fake.stops)
		}
		stop()
		if fake.stops != 1 {
			t.Fatalf("after one stop the seam's stop ran %d times, want 1", fake.stops)
		}
		stop()
		if fake.stops < 1 {
			t.Fatalf("after a second stop the seam's stop ran %d times, want at least 1", fake.stops)
		}
	})
}
