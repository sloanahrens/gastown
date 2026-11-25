package bdgateway

import (
	"context"
	"errors"
	"testing"
)

// TestExecutorAcquireSlot: on a one-slot executor acquireSlot takes the slot and
// returns a release that frees it; with the slot held the next acquire is
// refused at once by name, counts one refusal and returns no release.
func TestExecutorAcquireSlot(t *testing.T) {
	t.Parallel()
	newOneSlot := func(t *testing.T) *Executor {
		t.Helper()
		e, err := New(Options{
			Binary:        "/opt/bd/bd",
			WorkspaceRoot: testWorkspace(t),
			Home:          "/home/bdgw",
			Path:          "/usr/bin:/bin",
			DoltHost:      "gt-dolt",
			DoltPort:      "3307",
			MaxConcurrent: 1,
			runner:        &fakeRunner{},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return e
	}

	t.Run("free", func(t *testing.T) {
		t.Parallel()
		e := newOneSlot(t)
		release, err := e.acquireSlot(context.Background())
		if err != nil {
			t.Fatalf("acquireSlot on a free executor: %v", err)
		}
		if release == nil {
			t.Fatal("acquireSlot returned a nil release for a taken slot")
		}
		if got := e.CapStats().InFlight; got != 1 {
			t.Errorf("in flight while held = %d, want 1", got)
		}
		release()
		if got := e.CapStats().InFlight; got != 0 {
			t.Errorf("in flight after release = %d, want 0", got)
		}
		again, err := e.acquireSlot(context.Background())
		if err != nil {
			t.Fatalf("acquireSlot after release: %v", err)
		}
		again()
	})

	t.Run("full", func(t *testing.T) {
		t.Parallel()
		e := newOneSlot(t)
		held, err := e.acquireSlot(context.Background())
		if err != nil {
			t.Fatalf("first acquireSlot: %v", err)
		}
		defer held()

		release, err := e.acquireSlot(context.Background())
		if !errors.Is(err, ErrTooManyCalls) {
			t.Fatalf("acquireSlot on a full executor = %v, want ErrTooManyCalls", err)
		}
		if release != nil {
			t.Error("acquireSlot on a full executor returned a release, want nil")
		}
		if got := e.CapStats().TooManyCalls; got != 1 {
			t.Errorf("too_many_calls after one refusal = %d, want 1", got)
		}
	})
}
