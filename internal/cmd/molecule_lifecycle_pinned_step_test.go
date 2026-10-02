package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// pinStep leaves step carrying the hand-off pin mol-polecat-work used to set
// on the step it handed to the next session, held by an agent other than the
// one sweeping the molecule (bd refuses to close a pinned issue whoever holds
// it, and refuses a close by an actor the issue is not assigned to).
func pinStep(t *testing.T, fake *beadsfake.Fake, id string) {
	t.Helper()
	pinned, heldBy := string(beads.IssueStatusPinned), "gastown/polecats/emerald"
	if err := fake.Update(id, beads.UpdateOptions{Status: &pinned, Assignee: &heldBy}); err != nil {
		t.Fatalf("pin %s: %v", id, err)
	}
}

// TestCloseStepsThenRootReleasesAPinnedStep: gt done closes a finished
// molecule's steps unforced, and bd refuses to close a pinned issue. A step
// still carrying the hand-off pin the old `gt mol step done` left is not
// unfinished work, so the sweep releases the pin and closes the step instead
// of reporting it open and stranding the molecule and every step behind it
// until the reaper (gt-z5m6j).
func TestCloseStepsThenRootReleasesAPinnedStep(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	first, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: root.ID})
	if err != nil {
		t.Fatalf("create first step: %v", err)
	}
	second, err := fake.Create(beads.CreateOptions{Title: "branch-setup", Parent: root.ID})
	if err != nil {
		t.Fatalf("create second step: %v", err)
	}
	// A pinned step one level down, so the release is exercised by the
	// recursive close and not only by the level gt done sweeps first.
	deep, err := fake.Create(beads.CreateOptions{Title: "verify", Parent: first.ID})
	if err != nil {
		t.Fatalf("create nested step: %v", err)
	}
	for _, id := range []string{second.ID, deep.ID} {
		pinStep(t, fake, id)
	}

	rootClosed := false
	if _, err := closeStepsThenRoot(fake, root.ID, func() error {
		rootClosed = true
		return nil
	}); err != nil {
		t.Fatalf("closeStepsThenRoot = %v, want the molecule closed", err)
	}
	if !rootClosed {
		t.Error("closeStepsThenRoot left the root open; every step had closed")
	}
	for _, id := range []string{first.ID, second.ID, deep.ID} {
		got, err := fake.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != string(beads.StatusClosed) {
			t.Errorf("step %s status %q, want closed", id, got.Status)
		}
	}
}

// TestCloseStepsThenRootKeepsAStrandedPinnedStepVisible: releasing the pin is
// all the sweep does. A step bd refuses for a reason that outlives the pin —
// an open blocker outside the molecule — still stays open, and so does the
// root, so unfinished work stays visible (gt-7lx3).
func TestCloseStepsThenRootKeepsAStrandedPinnedStepVisible(t *testing.T) {
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
	pinStep(t, fake, step.ID)

	rootClosed := false
	_, err = closeStepsThenRoot(fake, root.ID, func() error {
		rootClosed = true
		return nil
	})
	if err == nil {
		t.Fatal("closeStepsThenRoot closed a molecule with a blocked step")
	}
	if !strings.Contains(err.Error(), step.ID) {
		t.Errorf("error = %v, want it to name the open step %s", err, step.ID)
	}
	if rootClosed {
		t.Error("closeStepsThenRoot closed the root over an open step")
	}
	if got, err := fake.Show(step.ID); err != nil || got.Status == string(beads.StatusClosed) {
		t.Errorf("step = %+v, %v; want it left open", got, err)
	}
}
