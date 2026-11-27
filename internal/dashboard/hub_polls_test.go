package dashboard

import (
	"context"
	"sync"
	"testing"
	"time"
)

// stopwatch is the hub clock a test moves by hand: a poll's wait and run times
// are read from it, so the test states them rather than waiting for them. arm
// makes the next Now call signal started, which tells the test that a poll has
// read its start time and is about to wait for a pollGate slot.
type stopwatch struct {
	mu      sync.Mutex
	now     time.Time
	started chan struct{}
}

func newStopwatch() *stopwatch {
	return &stopwatch{now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *stopwatch) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started != nil {
		c.started <- struct{}{}
		c.started = nil
	}
	return c.now
}

func (c *stopwatch) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *stopwatch) arm() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = make(chan struct{}, 1)
	return c.started
}

// timingPoll is a poll whose work the test states: it advances the clock by run
// and closes ran.
type timingPoll struct {
	clk *stopwatch
	run time.Duration
	ran chan struct{}
}

func (p *timingPoll) poll() {
	p.clk.advance(p.run)
	close(p.ran)
}

// runPoll keeps each poll's wait for a pollGate slot and its run time in
// State.Polls under the poll's method name (gt-uywol3). A clock the test states
// drives both times, so a poll that finds a slot free waits none while a fourth
// poll behind three held slots waits exactly what the clock advanced while it
// stood there.
func TestHubRecordsPollTiming(t *testing.T) {
	t.Parallel()

	t.Run("a free slot waits none and the run time is recorded", func(t *testing.T) {
		t.Parallel()

		clk := newStopwatch()
		h := NewHub(Config{Now: clk.Now})
		probe := &timingPoll{clk: clk, run: 40 * time.Millisecond, ran: make(chan struct{})}

		h.runPoll(context.Background(), probe.poll)
		wantSignal(t, probe.ran, "runPoll never ran the poll")

		timing, ok := h.State().Polls["poll"]
		if !ok {
			t.Fatalf("runPoll recorded no timing: Polls = %+v", h.State().Polls)
		}
		if timing.WaitMS != 0 {
			t.Errorf("WaitMS = %d, want 0: the poll found a slot free", timing.WaitMS)
		}
		if timing.RunMS != 40 {
			t.Errorf("RunMS = %d, want 40", timing.RunMS)
		}
	})

	t.Run("a slot held elsewhere is waited out and recorded", func(t *testing.T) {
		t.Parallel()

		clk := newStopwatch()
		h := NewHub(Config{Now: clk.Now})
		for i := 0; i < maxParallelPolls; i++ {
			h.pollGate <- struct{}{}
		}
		probe := &timingPoll{clk: clk, run: 40 * time.Millisecond, ran: make(chan struct{})}
		started := clk.arm()
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.runPoll(context.Background(), probe.poll)
		}()
		wantSignal(t, started, "the fourth poll never read its wait start")
		clk.advance(250 * time.Millisecond)
		wantSignal(t, h.pollGate, "no slot to free") // the fourth poll takes it and runs
		wantSignal(t, done, "the fourth poll never returned")

		timing, ok := h.State().Polls["poll"]
		if !ok {
			t.Fatalf("runPoll recorded no timing: Polls = %+v", h.State().Polls)
		}
		if timing.WaitMS != 250 {
			t.Errorf("WaitMS = %d, want 250: the poll stood behind three held slots that long", timing.WaitMS)
		}
		if timing.RunMS != 40 {
			t.Errorf("RunMS = %d, want 40", timing.RunMS)
		}
	})

	t.Run("a method value is recorded under its method name", func(t *testing.T) {
		t.Parallel()

		h := NewHub(Config{
			Now:    newStopwatch().Now,
			Health: func() Health { return Health{} },
		})

		h.runPoll(context.Background(), h.pollHealth)

		if _, ok := h.State().Polls["pollHealth"]; !ok {
			t.Fatalf("Polls = %+v, want an entry keyed pollHealth", h.State().Polls)
		}
	})
}
