package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specStoreExit is a bd failure whose process exited with status 25, the
// store_unavailable exit, as the exec error bd leaves behind carries it.
type specStoreExit struct{}

func (specStoreExit) Error() string { return "exit status 25" }
func (specStoreExit) ExitCode() int { return 25 }

// TestSpecDispatchStoreUnavailableIsADeferral: a sling that fails because the
// store did not answer is a transient read failure, not a defect of the bead.
// The dispatcher defers it to the next tick and adds neither the failed label
// nor a note, so a stall does not hold a P0 bead out of the queue until a
// person clears it (gt-m7h3xn). An ordinary sling failure still labels the bead.
func TestSpecDispatchStoreUnavailableIsADeferral(t *testing.T) {
	t.Parallel()
	storeDown := fmt.Errorf("instantiating formula mol-polecat-work: %w",
		fmt.Errorf("dep list gt-cl31e9: %w", beads.ErrStoreUnavailable))
	cases := []struct {
		name         string
		err          error
		wantFailed   int
		wantDeferred int
		wantLabel    bool
	}{
		{"store_unavailable_defers", storeDown, 0, 1, false},
		{"exit_25_defers", fmt.Errorf("dep list gt-cl31e9: %w", specStoreExit{}), 0, 1, false},
		{"another_failure_labels", errors.New("instantiating formula mol-polecat-work: unknown var"), 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
			f.slingErrs["gt-a"] = []error{tc.err}
			r := runSpecDispatchCycle(f.env())
			if len(r.Failed) != tc.wantFailed || len(r.Deferred) != tc.wantDeferred || len(f.slung) != 0 {
				t.Fatalf("failed %+v deferred %+v slung %v; want %d failed and %d deferred", r.Failed, r.Deferred, f.slung, tc.wantFailed, tc.wantDeferred)
			}
			if got := slices.Contains(f.labels["gt-a"], specdispatch.DispatchFailedLabel); got != tc.wantLabel {
				t.Errorf("labels = %v, want %s added = %v", f.labels["gt-a"], specdispatch.DispatchFailedLabel, tc.wantLabel)
			}
			if tc.wantLabel {
				return
			}
			if n := len(f.notes["gt-a"]); n != 0 {
				t.Errorf("annotated %d times, want none: the bead is not held out of the queue", n)
			}
			if line := r.Deferred[0].Line; !strings.Contains(line, "the store did not answer") || !strings.Contains(line, tc.err.Error()) {
				t.Errorf("deferral line = %q, want it to name the store and the first error line", line)
			}
			// Still a candidate: the next tick slings it once the store answers.
			if r := runSpecDispatchCycle(f.env()); len(r.Dispatched) != 1 || len(f.slung) != 1 {
				t.Errorf("next tick: dispatched %+v, slung %v; want the bead slung", r.Dispatched, f.slung)
			}
		})
	}
}

// TestSpecDispatchOrdinaryFailureStillLabels: the store deferral of gt-m7h3xn
// must not widen to every sling failure. An ordinary sling error, a wrapped
// context deadline, still labels the bead spec-dispatch-failed; a bond lock
// timeout keeps its own deferral and is not labelled (gt-8d38p).
func TestSpecDispatchOrdinaryFailureStillLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		err          error
		wantFailed   int
		wantDeferred int
		wantLabel    bool
	}{
		{"ordinary_error_labels", errors.New("instantiating formula mol-polecat-work: unknown var"), 1, 0, true},
		{"bond_lock_timeout_defers", fmt.Errorf("instantiating formula mol-polecat-work: %w",
			fmt.Errorf("bonding: %w", ErrBondLockTimeout)), 0, 1, false},
		{"wrapped_deadline_labels", fmt.Errorf("dep list gt-cl31e9: %w", context.DeadlineExceeded), 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
			f.slingErrs["gt-a"] = []error{tc.err}
			r := runSpecDispatchCycle(f.env())
			if len(r.Failed) != tc.wantFailed || len(r.Deferred) != tc.wantDeferred || len(f.slung) != 0 {
				t.Fatalf("failed %+v deferred %+v slung %v; want %d failed and %d deferred", r.Failed, r.Deferred, f.slung, tc.wantFailed, tc.wantDeferred)
			}
			if got := slices.Contains(f.labels["gt-a"], specdispatch.DispatchFailedLabel); got != tc.wantLabel {
				t.Errorf("labels = %v, want %s added = %v", f.labels["gt-a"], specdispatch.DispatchFailedLabel, tc.wantLabel)
			}
		})
	}
}
