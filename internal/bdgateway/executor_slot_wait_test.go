package bdgateway

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestExecutorSlotWaitOption: a zero SlotWait takes DefaultSlotWait, a positive
// one is kept as given, and a negative one is refused by name like the other
// durations.
func TestExecutorSlotWaitOption(t *testing.T) {
	t.Parallel()
	base := Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: "/ws",
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
	}
	cases := []struct {
		name     string
		slotWait time.Duration
		want     time.Duration
		refused  bool
	}{
		{name: "zero takes the default", slotWait: 0, want: DefaultSlotWait},
		{name: "positive is kept", slotWait: 3 * time.Second, want: 3 * time.Second},
		{name: "negative is refused", slotWait: -time.Second, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.SlotWait = tc.slotWait
			e, err := New(opts)
			if tc.refused {
				if !errors.Is(err, ErrUnconfigured) {
					t.Fatalf("New = %v, want ErrUnconfigured", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if e.slotWait != tc.want {
				t.Errorf("slot wait = %s, want %s", e.slotWait, tc.want)
			}
		})
	}
}

// signalAfter is a timer seam whose timer never fires and which tells the test
// each time the executor starts one, so a test knows a call is waiting without
// any real wait.
type signalAfter struct {
	started chan time.Duration
}

func (s *signalAfter) after(d time.Duration) (<-chan time.Time, func() bool) {
	s.started <- d
	return make(chan time.Time), func() bool { return true }
}

// TestExecutorSlotWaitRunsWhenASlotFrees: with every slot taken a second call
// waits for one, runs when the first ends, and is not counted as a refusal.
func TestExecutorSlotWaitRunsWhenASlotFrees(t *testing.T) {
	t.Parallel()
	proc := newBlockingFake("")
	runner := &fakeRunner{queue: []Process{proc}, started: make(chan struct{}), bound: waitCtx(t)}
	timer := &signalAfter{started: make(chan time.Duration, 1)}
	e, err := New(Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: testWorkspace(t),
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
		MaxConcurrent: 1,
		SlotWait:      7 * time.Second,
		runner:        runner,
		after:         timer.after,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &bytes.Buffer{})
	}()
	select {
	case <-runner.started:
	case <-waitCtx(t).Done():
		t.Fatal("bd was not started within the test's bound")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &bytes.Buffer{})
	}()
	select {
	case d := <-timer.started:
		if d != 7*time.Second {
			t.Errorf("the wait started a timer of %v, want the slot wait of 7s", d)
		}
	case err := <-secondDone:
		t.Fatalf("the second call returned %v while the slot was held, want it waiting", err)
	case <-waitCtx(t).Done():
		t.Fatal("the second call did not start waiting within the test's bound")
	}

	proc.finish(0)
	for _, done := range []chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("a call ended with %v, want nil", err)
			}
		case <-waitCtx(t).Done():
			t.Fatal("a call did not end within the test's bound")
		}
	}
	if got := e.CapStats().TooManyCalls; got != 0 {
		t.Errorf("too_many_calls = %d, want 0: a call that got a slot is not a refusal", got)
	}
}

// stopCountingAfter is a timer seam that counts the stops of its timers. A fired
// timer's channel is closed, so its wait ends at once; any other never fires.
type stopCountingAfter struct {
	fired   bool
	started chan struct{}
	stops   atomic.Int32
}

func (s *stopCountingAfter) after(time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time)
	if s.fired {
		close(ch)
	}
	s.started <- struct{}{}
	return ch, func() bool {
		s.stops.Add(1)
		return true
	}
}

// TestExecutorSlotWaitStopsItsTimer: a call whose wait starts a timer stops it
// once on every return, whether the wait ends in a slot or in a refusal.
func TestExecutorSlotWaitStopsItsTimer(t *testing.T) {
	t.Parallel()
	// setup builds a one-slot executor on timer; the first call holds its slot.
	setup := func(t *testing.T, timer *stopCountingAfter) (*Executor, *fakeRunner, *fakeProcess) {
		t.Helper()
		proc := newBlockingFake("")
		runner := &fakeRunner{queue: []Process{proc}, started: make(chan struct{}), bound: waitCtx(t)}
		e, err := New(Options{
			Binary:        "/opt/bd/bd",
			WorkspaceRoot: testWorkspace(t),
			Home:          "/home/bdgw",
			Path:          "/usr/bin:/bin",
			DoltHost:      "gt-dolt",
			DoltPort:      "3307",
			MaxConcurrent: 1,
			SlotWait:      7 * time.Second,
			runner:        runner,
			after:         timer.after,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return e, runner, proc
	}
	// call runs one Exec in the background; its result arrives on the channel.
	call := func(e *Executor) chan error {
		done := make(chan error, 1)
		go func() {
			done <- e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &bytes.Buffer{})
		}()
		return done
	}
	// drain waits for each call to end with nil, within the test's bound.
	drain := func(t *testing.T, calls ...chan error) {
		t.Helper()
		for _, done := range calls {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("a call ended with %v, want nil", err)
				}
			case <-waitCtx(t).Done():
				t.Fatal("a call did not end within the test's bound")
			}
		}
	}

	t.Run("a call that gets a slot after waiting stops its timer once", func(t *testing.T) {
		t.Parallel()
		timer := &stopCountingAfter{started: make(chan struct{}, 1)}
		e, runner, proc := setup(t, timer)
		first := call(e)
		select {
		case <-runner.started:
		case <-waitCtx(t).Done():
			t.Fatal("bd was not started within the test's bound")
		}
		second := call(e)
		select {
		case <-timer.started:
		case <-waitCtx(t).Done():
			t.Fatal("the second call did not start waiting within the test's bound")
		}
		proc.finish(0)
		drain(t, first, second)
		if got := timer.stops.Load(); got != 1 {
			t.Errorf("stop func called %d times, want 1", got)
		}
	})

	t.Run("a call refused at once stops its timer once", func(t *testing.T) {
		t.Parallel()
		timer := &stopCountingAfter{fired: true, started: make(chan struct{}, 1)}
		e, runner, proc := setup(t, timer)
		first := call(e)
		select {
		case <-runner.started:
		case <-waitCtx(t).Done():
			t.Fatal("bd was not started within the test's bound")
		}

		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &bytes.Buffer{})
		if !errors.Is(err, ErrTooManyCalls) {
			t.Errorf("refused call = %v, want ErrTooManyCalls", err)
		}
		if got := timer.stops.Load(); got != 1 {
			t.Errorf("stop func called %d times, want 1", got)
		}

		proc.finish(0)
		drain(t, first)
	})
}
