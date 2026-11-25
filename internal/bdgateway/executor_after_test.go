package bdgateway

import (
	"testing"
	"time"
)

// fakeAfter is the timer seam a test injects in place of the default. It counts
// its calls and records each duration. Its channel is never fired, so no row
// waits on it.
type fakeAfter struct {
	calls     int
	durations []time.Duration
}

func (f *fakeAfter) after(d time.Duration) (<-chan time.Time, func()) {
	f.calls++
	f.durations = append(f.durations, d)
	return make(chan time.Time), func() {}
}

// newAfterExecutor builds an executor with the given timer seam. A nil seam
// leaves the option unset, so the executor takes its default.
func newAfterExecutor(t *testing.T, after func(time.Duration) (<-chan time.Time, func())) *Executor {
	t.Helper()
	e, err := New(Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: t.TempDir(),
		Home:          "/home/bdgw",
		Path:          "/usr/local/bin:/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
		after:         after,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestExecutorAfterSeam(t *testing.T) {
	t.Parallel()
	t.Run("an injected seam is kept and called", func(t *testing.T) {
		fake := &fakeAfter{}
		e := newAfterExecutor(t, fake.after)
		const d = 7 * time.Second
		_, stop := e.after(d)
		stop()
		if fake.calls != 1 {
			t.Fatalf("injected seam calls = %d, want 1", fake.calls)
		}
		if len(fake.durations) != 1 || fake.durations[0] != d {
			t.Fatalf("injected seam durations = %v, want [%v]", fake.durations, d)
		}
	})

	t.Run("the default fires a zero duration at once", func(t *testing.T) {
		e := newAfterExecutor(t, nil)
		if e.after == nil {
			t.Fatal("default seam is nil, want a timer wrapper")
		}
		ch, stop := e.after(0)
		defer stop()
		// A zero-duration timer is already due, so the receive returns at once.
		// The bound only ends a hang: a timer that never fires fails the row.
		ctx := waitCtx(t)
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("default timer with a zero duration did not fire")
		}
	})

	t.Run("the default stop leaves the channel empty", func(t *testing.T) {
		e := newAfterExecutor(t, nil)
		ch, stop := e.after(time.Hour)
		stop()
		// The hour cannot elapse in a test, so an empty non-blocking receive is
		// the check: nothing waits here.
		select {
		case <-ch:
			t.Fatal("default timer fired after its stop func ran")
		default:
		}
	})
}
