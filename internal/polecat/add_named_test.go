package polecat

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestAddNamedWithOptions_ReservesNameBeforeCreating guards the gt-2w4f9
// review finding: a named create (gt sling <bead> <rig>/<name> --create) must
// hold the name under the pool lock before the slow worktree build, or a
// concurrent unnamed sling's AllocateAndAdd can be handed the same name and
// destroy the new polecat on its error path. The hook runs after the pool
// lock is released and before the worktree exists — the race window — and
// asks the allocator for a name there.
func TestAddNamedWithOptions_ReservesNameBeforeCreating(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)

	// The name the pool would hand out next.
	next, err := mgr.AllocateName()
	if err != nil {
		t.Fatalf("AllocateName: %v", err)
	}
	_ = os.Remove(mgr.pendingPath(next))
	mgr.ReleaseName(next)

	// The concurrent allocation runs on its own goroutine, as a second gt
	// process would. Once it has seen the reserved directory it waits on the
	// named polecat's lock (reconcile's orphan sweep), so it may only finish
	// after the named create; without the reservation it finishes at once —
	// with the same name.
	type result struct {
		name string
		err  error
	}
	done := make(chan result, 1)
	var early *result
	prev := afterNamedPolecatReserved
	t.Cleanup(func() { afterNamedPolecatReserved = prev })
	afterNamedPolecatReserved = func(name string) {
		go func() {
			n, err := mgr.AllocateName()
			done <- result{n, err}
		}()
		select {
		case r := <-done:
			early = &r
		case <-time.After(2 * time.Second):
		}
	}

	p, err := mgr.AddNamedWithOptions(next, AddOptions{})
	if err != nil {
		t.Fatalf("AddNamedWithOptions(%s): %v", next, err)
	}
	if p.Name != next {
		t.Fatalf("created %q; want %q", p.Name, next)
	}
	r := early
	if r == nil {
		select {
		case got := <-done:
			r = &got
		case <-time.After(30 * time.Second):
			t.Fatal("concurrent AllocateName never finished")
		}
	}
	if r.err != nil {
		t.Fatalf("concurrent AllocateName: %v", r.err)
	}
	if r.name == next {
		t.Fatalf("a concurrent allocation was handed %q while the named create was building it", next)
	}
}

func TestAddNamedWithOptions_RefusesExistingAndInvalidNames(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	if _, err := mgr.AddNamedWithOptions("toast", AddOptions{}); err != nil {
		t.Fatalf("AddNamedWithOptions(toast): %v", err)
	}
	if _, err := mgr.AddNamedWithOptions("toast", AddOptions{}); !errors.Is(err, ErrPolecatExists) {
		t.Fatalf("second create err = %v; want ErrPolecatExists", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "nux", "Toast", "witness"} {
		if _, err := mgr.AddNamedWithOptions(bad, AddOptions{}); err == nil {
			t.Errorf("AddNamedWithOptions(%q) succeeded; want an invalid-name refusal", bad)
		}
	}
}
