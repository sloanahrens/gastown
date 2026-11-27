package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestStepWispRetireZeroStepsClosesRootOnce: a root-only molecule with no
// steps closes through closeRoot exactly once, reports zero steps closed, and
// returns no error. A closeRoot failure is returned, not swallowed.
func TestStepWispRetireZeroStepsClosesRootOnce(t *testing.T) {
	t.Parallel()
	boom := errors.New("root close failed")
	tests := []struct {
		name     string
		closeErr error
		wantErr  error
	}{
		{"closes the root", nil, nil},
		{"reports the root close failure", boom, boom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := beadsfake.New()
			root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
			if err != nil {
				t.Fatalf("create root: %v", err)
			}

			calls := 0
			steps, err := closeStepsThenRoot(fake, root.ID, func() error {
				calls++
				return tc.closeErr
			})
			if steps != 0 {
				t.Errorf("steps closed = %d, want 0", steps)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("closeStepsThenRoot error = %v, want %v", err, tc.wantErr)
			}
			if calls != 1 {
				t.Errorf("closeRoot called %d times, want 1", calls)
			}
		})
	}
}

// TestStepWispRetireOpenStepKeepsRootOpen: a molecule with one step bd refuses
// (blocked by an open issue outside the molecule) stays open. The error names
// the step, closeRoot is never called, and the step is left open. The refusal
// is what keeps unfinished work visible (gt-7lx3).
func TestStepWispRetireOpenStepKeepsRootOpen(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := fake.Create(beads.CreateOptions{Title: "implement", Parent: root.ID})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	blocker, err := fake.Create(beads.CreateOptions{Title: "open blocker"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := fake.AddDependency(step.ID, blocker.ID); err != nil {
		t.Fatalf("block the step: %v", err)
	}

	rootClosed := false
	steps, err := closeStepsThenRoot(fake, root.ID, func() error {
		rootClosed = true
		return nil
	})
	if err == nil {
		t.Fatal("closeStepsThenRoot closed a molecule with an open step")
	}
	if !strings.Contains(err.Error(), step.ID) {
		t.Errorf("error = %v, want it to name the open step %s", err, step.ID)
	}
	if steps != 0 {
		t.Errorf("steps closed = %d, want 0 with the only step refused", steps)
	}
	if rootClosed {
		t.Error("closeStepsThenRoot closed the root over an open step")
	}
	if got, err := fake.Show(step.ID); err != nil || got.Status == string(beads.StatusClosed) {
		t.Errorf("step = %+v, %v; want it left open", got, err)
	}
}

// TestStepWispRetireDoneReportsOnlyClosedSteps: gt done prints the steps-closed
// line only when it closed at least one step. A molecule with no steps closes
// its root silently, with no warning; a molecule with one step reports it.
func TestStepWispRetireDoneReportsOnlyClosedSteps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		hasStep  bool
		wantLine string
	}{
		{"no steps prints nothing", false, ""},
		{"one step prints its count", true, "Closed 1 molecule step(s) for gt-wisp-xyz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := doneMoleculeDB(true)
			if tc.hasStep {
				seedChildren(t, db, "gt-wisp-xyz", beads.Issue{ID: "gt-wisp-xyz-step-1", Title: "Step 1", Status: "open", Ephemeral: true})
			}
			rec := &doneRecorder{Client: db, mu: &sync.Mutex{}, closes: &[]string{}}
			var told []string
			if _, err := runDoneStateUpdateReporting(t, rec, ExitCompleted, collectMessages(&told)); err != nil {
				t.Fatalf("gt done: %v", err)
			}

			got := strings.Join(told, "\n")
			if got != tc.wantLine {
				t.Errorf("gt done reported %q, want %q", got, tc.wantLine)
			}
			if !slices.Contains(rec.closed(), "gt-wisp-xyz") {
				t.Errorf("closes = %v, want the molecule root closed", rec.closed())
			}
		})
	}
}

// TestStepWispRetireDoneWarnsWhenStepOpen: an attached molecule whose step bd
// refuses to close (blocked by an open issue outside the molecule) is left
// open, and gt done says so on its operator-message seam. No steps-closed line
// is printed for the zero count, and neither the root nor the base bead is
// closed: the polecat stands down with the molecule still visible.
func TestStepWispRetireDoneWarnsWhenStepOpen(t *testing.T) {
	t.Parallel()
	db := doneMoleculeDB(true)
	seedChildren(t, db, "gt-wisp-xyz", beads.Issue{ID: "gt-wisp-xyz-step-1", Title: "Step 1", Status: "open", Ephemeral: true})
	db.Seed(beads.Issue{ID: "gt-blocker", Title: "open blocker", Status: "open"})
	if err := db.AddDependency("gt-wisp-xyz-step-1", "gt-blocker"); err != nil {
		t.Fatalf("block the step: %v", err)
	}

	rec := &doneRecorder{Client: db, mu: &sync.Mutex{}, closes: &[]string{}}
	var told []string
	if _, err := runDoneStateUpdateReporting(t, rec, ExitCompleted, collectMessages(&told)); err != nil {
		t.Fatalf("gt done: %v", err)
	}

	joined := strings.Join(told, "\n")
	if !strings.Contains(joined, "Warning: couldn't close attached molecule gt-wisp-xyz:") {
		t.Errorf("gt done reported %q, want the molecule close warning", joined)
	}
	if strings.Contains(joined, "Closed ") {
		t.Errorf("gt done reported a steps-closed line for a refused close: %q", joined)
	}
	for _, id := range []string{"gt-wisp-xyz", "gt-base-123"} {
		if slices.Contains(rec.closed(), id) {
			t.Errorf("closes = %v, want %s left open", rec.closed(), id)
		}
		if issue, err := db.Show(id); err != nil || issue.Status == string(beads.StatusClosed) {
			t.Errorf("%s = %+v, %v; want it left open", id, issue, err)
		}
	}
	if issue, err := db.Show("gt-wisp-xyz-step-1"); err != nil || issue.Status == string(beads.StatusClosed) {
		t.Errorf("step = %+v, %v; want it left open", issue, err)
	}
}

// collectMessages is the operator-message seam that appends each line gt done
// prints to told.
func collectMessages(told *[]string) func(format string, args ...any) {
	return func(format string, args ...any) {
		*told = append(*told, fmt.Sprintf(format, args...))
	}
}
