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

func (f *fakeAfter) after(d time.Duration) (<-chan time.Time, func() bool) {
	f.calls++
	f.durations = append(f.durations, d)
	return make(chan time.Time), func() bool { return true }
}

// newAfterExecutor builds an executor with the given timer seam. A nil seam
// leaves the option unset, so the executor takes its default.
func newAfterExecutor(t *testing.T, after func(time.Duration) (<-chan time.Time, func() bool)) *Executor {
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

	t.Run("the default stop reports whether it stopped a live timer", func(t *testing.T) {
		e := newAfterExecutor(t, nil)
		if e.after == nil {
			t.Fatal("default seam is nil, want a timer wrapper")
		}
		// The hour cannot elapse in a test, so the timer stays live until the
		// first stop and nothing here waits on the channel.
		_, stop := e.after(time.Hour)
		if !stop() {
			t.Fatal("first stop returned false, want true for a live timer")
		}
		if stop() {
			t.Fatal("second stop returned true, want false for an already stopped timer")
		}
	})
}
