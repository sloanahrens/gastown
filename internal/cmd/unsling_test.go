package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// gt hook shows hooked and in_progress beads as the agent's work, so gt
// unsling must select both. Before it shared activeWorkStatuses it listed
// status hooked only and answered "Nothing on your hook" for an in_progress bead.
func TestUnslingFindsActiveHookedWork(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/p1"

	for _, tc := range []struct {
		name   string
		status beads.IssueStatus
		want   bool
	}{
		{"hooked", beads.IssueStatusHooked, true},
		{"in_progress", beads.StatusInProgress, true},
		{"open", beads.StatusOpen, false},
		{"closed", beads.StatusClosed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := beadsfake.New()
			is, err := store.Create(beads.CreateOptions{Title: "work", Assignee: agent})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			status := string(tc.status)
			if err := store.Update(is.ID, beads.UpdateOptions{Status: &status}); err != nil {
				t.Fatalf("Update to %s: %v", tc.status, err)
			}

			got := firstActiveWorkID(store, agent)
			if want := map[bool]string{true: is.ID, false: ""}[tc.want]; got != want {
				t.Errorf("firstActiveWorkID with a %s bead = %q, want %q", tc.status, got, want)
			}
		})
	}
}

// A bead assigned to another agent is not selected, and a hooked bead wins over
// an in_progress one the same agent also holds.
func TestUnslingPrefersHookedOverInProgress(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/p1"
	store := beadsfake.New()

	create := func(assignee string, status beads.IssueStatus) string {
		t.Helper()
		is, err := store.Create(beads.CreateOptions{Title: "work", Assignee: assignee})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		s := string(status)
		if err := store.Update(is.ID, beads.UpdateOptions{Status: &s}); err != nil {
			t.Fatalf("Update to %s: %v", status, err)
		}
		return is.ID
	}

	create("gastown/polecats/p2", beads.IssueStatusHooked)
	create(agent, beads.StatusInProgress)
	hooked := create(agent, beads.IssueStatusHooked)

	if got := firstActiveWorkID(store, agent); got != hooked {
		t.Errorf("firstActiveWorkID = %q, want the hooked bead %q", got, hooked)
	}
}
