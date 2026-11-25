package bdgateway

import (
	"bytes"
	"context"
	"errors"
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
