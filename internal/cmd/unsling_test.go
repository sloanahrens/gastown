package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

			got, err := firstActiveWorkID(store, agent)
			if err != nil {
				t.Fatalf("firstActiveWorkID: %v", err)
			}
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

	got, err := firstActiveWorkID(store, agent)
	if err != nil {
		t.Fatalf("firstActiveWorkID: %v", err)
	}
	if got != hooked {
		t.Errorf("firstActiveWorkID = %q, want the hooked bead %q", got, hooked)
	}
}

// brokenStore fails the calls gt unsling makes, to cover its error paths.
type brokenStore struct {
	beads.Client
	listErr   error
	updateErr error
}

func (s brokenStore) ListAssignedIssueStatuses(assignee string, statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Client.ListAssignedIssueStatuses(assignee, statuses...)
}

func (s brokenStore) Update(id string, opts beads.UpdateOptions) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Client.Update(id, opts)
}

// A failed lookup must reach the caller: reported as an empty hook, gt unsling
// would say "Nothing on your hook" while the agent still holds work.
func TestUnslingLookupErrorsAreReturned(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/p1"
	boom := errors.New("bd list: connection refused")
	store := brokenStore{Client: beadsfake.New(), listErr: boom}

	if id, err := firstActiveWorkID(store, agent); !errors.Is(err, boom) || id != "" {
		t.Errorf("firstActiveWorkID = (%q, %v), want (\"\", %v)", id, err, boom)
	}

	cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, agent, "", "", "", false)
	if !errors.Is(err, boom) || cleaned {
		t.Errorf("cleanStaleHookedBeads = (%v, %v), want (false, %v)", cleaned, err, boom)
	}
}

// seedAssigned creates a bead held by assignee in the given status.
func seedAssigned(t *testing.T, store beads.Client, assignee string, status beads.IssueStatus) *beads.Issue {
	t.Helper()
	is, err := store.Create(beads.CreateOptions{Title: "work", Assignee: assignee})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s := string(status)
	if err := store.Update(is.ID, beads.UpdateOptions{Status: &s}); err != nil {
		t.Fatalf("Update to %s: %v", status, err)
	}
	return is
}

func TestReopenBeadClearsStatusAndAssignee(t *testing.T) {
	t.Parallel()
	store := beadsfake.New()
	is := seedAssigned(t, store, "gastown/polecats/p1", beads.IssueStatusHooked)

	if err := reopenBead(store, is.ID); err != nil {
		t.Fatalf("reopenBead: %v", err)
	}
	got, err := store.Show(is.ID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got.Status != string(beads.StatusOpen) || got.Assignee != "" {
		t.Errorf("after reopenBead status=%q assignee=%q, want open and unassigned", got.Status, got.Assignee)
	}
}

func TestCleanStaleHookedBeads(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/p1"

	t.Run("reopens hooked and in_progress beads of the agent only", func(t *testing.T) {
		t.Parallel()
		store := beadsfake.New()
		hooked := seedAssigned(t, store, agent, beads.IssueStatusHooked)
		running := seedAssigned(t, store, agent, beads.StatusInProgress)
		other := seedAssigned(t, store, "gastown/polecats/p2", beads.IssueStatusHooked)

		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, agent, "", "", "", false)
		if err != nil || !cleaned {
			t.Fatalf("cleanStaleHookedBeads = (%v, %v), want (true, nil)", cleaned, err)
		}
		for _, id := range []string{hooked.ID, running.ID} {
			if got, _ := store.Show(id); got.Status != string(beads.StatusOpen) || got.Assignee != "" {
				t.Errorf("bead %s status=%q assignee=%q, want open and unassigned", id, got.Status, got.Assignee)
			}
		}
		if got, _ := store.Show(other.ID); got.Status != string(beads.IssueStatusHooked) || got.Assignee != "gastown/polecats/p2" {
			t.Errorf("another agent's bead changed: status=%q assignee=%q", got.Status, got.Assignee)
		}
	})

	t.Run("target bead id limits the cleanup", func(t *testing.T) {
		t.Parallel()
		store := beadsfake.New()
		want := seedAssigned(t, store, agent, beads.IssueStatusHooked)
		keep := seedAssigned(t, store, agent, beads.IssueStatusHooked)

		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, agent, want.ID, "", "", false)
		if err != nil || !cleaned {
			t.Fatalf("cleanStaleHookedBeads = (%v, %v), want (true, nil)", cleaned, err)
		}
		if got, _ := store.Show(want.ID); got.Status != string(beads.StatusOpen) {
			t.Errorf("target bead status=%q, want open", got.Status)
		}
		if got, _ := store.Show(keep.ID); got.Status != string(beads.IssueStatusHooked) {
			t.Errorf("untargeted bead status=%q, want it left hooked", got.Status)
		}

		cleaned, err = cleanStaleHookedBeads(&cobra.Command{}, store, agent, "gt-nosuch", "", "", false)
		if err != nil || cleaned {
			t.Errorf("unmatched target = (%v, %v), want (false, nil)", cleaned, err)
		}
	})

	t.Run("dry run reports without changing anything", func(t *testing.T) {
		t.Parallel()
		store := beadsfake.New()
		is := seedAssigned(t, store, agent, beads.IssueStatusHooked)

		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, agent, "", "", "", true)
		if err != nil || !cleaned {
			t.Fatalf("cleanStaleHookedBeads = (%v, %v), want (true, nil)", cleaned, err)
		}
		if got, _ := store.Show(is.ID); got.Status != string(beads.IssueStatusHooked) || got.Assignee != agent {
			t.Errorf("dry run changed the bead: status=%q assignee=%q", got.Status, got.Assignee)
		}
	})

	t.Run("nothing assigned", func(t *testing.T) {
		t.Parallel()
		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, beadsfake.New(), agent, "", "", "", false)
		if err != nil || cleaned {
			t.Errorf("cleanStaleHookedBeads = (%v, %v), want (false, nil)", cleaned, err)
		}
	})

	t.Run("a failed update warns and the next bead is still cleaned", func(t *testing.T) {
		t.Parallel()
		fake := beadsfake.New()
		first := seedAssigned(t, fake, agent, beads.IssueStatusHooked)
		seedAssigned(t, fake, agent, beads.IssueStatusHooked)
		store := brokenStore{Client: fake, updateErr: errors.New("bd update: locked")}

		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetErr(&stderr)
		cleaned, err := cleanStaleHookedBeads(cmd, store, agent, "", "", "", false)
		if err != nil || !cleaned {
			t.Fatalf("cleanStaleHookedBeads = (%v, %v), want (true, nil)", cleaned, err)
		}
		if got := strings.Count(stderr.String(), "couldn't clean up stale bead"); got != 2 {
			t.Errorf("stderr has %d update warnings, want 2:\n%s", got, stderr.String())
		}
		if got, _ := fake.Show(first.ID); got.Status != string(beads.IssueStatusHooked) {
			t.Errorf("bead whose update failed has status %q, want it unchanged", got.Status)
		}
	})
}
