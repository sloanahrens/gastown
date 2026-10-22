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
	// failAssignee limits listErr to lookups for that assignee; empty fails all.
	failAssignee string
}

func (s brokenStore) ListAssignedIssueStatuses(assignee string, statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	if s.listErr != nil && (s.failAssignee == "" || s.failAssignee == assignee) {
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

	cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, openBeads, agent, "", "", "", false)
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

// forceRecorder remembers the options of the last Update.
type forceRecorder struct {
	beads.Client
	last beads.UpdateOptions
}

func (s *forceRecorder) Update(id string, opts beads.UpdateOptions) error {
	s.last = opts
	return s.Client.Update(id, opts)
}

// The in_progress case is the one a plain update refuses (bd will not clear the
// assignee of an in_progress bead), so reopenBead must force it and the bead
// must end up open.
func TestReopenBeadClearsStatusAndAssignee(t *testing.T) {
	t.Parallel()
	for _, status := range []beads.IssueStatus{beads.IssueStatusHooked, beads.StatusInProgress} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			store := &forceRecorder{Client: beadsfake.New()}
			is := seedAssigned(t, store, "gastown/polecats/p1", status)

			if err := reopenBead(store, is.ID); err != nil {
				t.Fatalf("reopenBead: %v", err)
			}
			if !store.last.Force {
				t.Errorf("reopenBead updated without Force; a plain update cannot clear an in_progress claim")
			}
			got, err := store.Show(is.ID)
			if err != nil {
				t.Fatalf("Show: %v", err)
			}
			if got.Status != string(beads.StatusOpen) || got.Assignee != "" {
				t.Errorf("after reopenBead status=%q assignee=%q, want open and unassigned", got.Status, got.Assignee)
			}
		})
	}
}

// The town database is searched under the exact agent ID, then under the
// normalized identity mail beads use; a lookup failure under either is returned.
func TestFirstTownActiveWorkID(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/p1"
	const normalized = "gastown/p1"
	boom := errors.New("bd list: connection refused")

	t.Run("exact id", func(t *testing.T) {
		t.Parallel()
		store := beadsfake.New()
		is := seedAssigned(t, store, agent, beads.IssueStatusHooked)
		if got, err := firstTownActiveWorkID(store, agent); err != nil || got != is.ID {
			t.Errorf("= (%q, %v), want (%q, nil)", got, err, is.ID)
		}
	})

	t.Run("normalized id", func(t *testing.T) {
		t.Parallel()
		store := beadsfake.New()
		is := seedAssigned(t, store, normalized, beads.StatusInProgress)
		if got, err := firstTownActiveWorkID(store, agent); err != nil || got != is.ID {
			t.Errorf("= (%q, %v), want (%q, nil)", got, err, is.ID)
		}
	})

	t.Run("nothing hooked", func(t *testing.T) {
		t.Parallel()
		if got, err := firstTownActiveWorkID(beadsfake.New(), agent); err != nil || got != "" {
			t.Errorf("= (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("exact lookup fails", func(t *testing.T) {
		t.Parallel()
		store := brokenStore{Client: beadsfake.New(), listErr: boom, failAssignee: agent}
		got, err := firstTownActiveWorkID(store, agent)
		if !errors.Is(err, boom) || got != "" || !strings.Contains(err.Error(), agent) {
			t.Errorf("= (%q, %v), want (\"\", an error wrapping %v naming %s)", got, err, boom, agent)
		}
	})

	t.Run("normalized lookup fails", func(t *testing.T) {
		t.Parallel()
		store := brokenStore{Client: beadsfake.New(), listErr: boom, failAssignee: normalized}
		got, err := firstTownActiveWorkID(store, agent)
		if !errors.Is(err, boom) || got != "" || !strings.Contains(err.Error(), normalized) {
			t.Errorf("= (%q, %v), want (\"\", an error wrapping %v naming %s)", got, err, boom, normalized)
		}
	})
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

		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, openBeads, agent, "", "", "", false)
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

		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, openBeads, agent, want.ID, "", "", false)
		if err != nil || !cleaned {
			t.Fatalf("cleanStaleHookedBeads = (%v, %v), want (true, nil)", cleaned, err)
		}
		if got, _ := store.Show(want.ID); got.Status != string(beads.StatusOpen) {
			t.Errorf("target bead status=%q, want open", got.Status)
		}
		if got, _ := store.Show(keep.ID); got.Status != string(beads.IssueStatusHooked) {
			t.Errorf("untargeted bead status=%q, want it left hooked", got.Status)
		}

		cleaned, err = cleanStaleHookedBeads(&cobra.Command{}, store, openBeads, agent, "gt-nosuch", "", "", false)
		if err != nil || cleaned {
			t.Errorf("unmatched target = (%v, %v), want (false, nil)", cleaned, err)
		}
	})

	t.Run("dry run reports without changing anything", func(t *testing.T) {
		t.Parallel()
		store := beadsfake.New()
		is := seedAssigned(t, store, agent, beads.IssueStatusHooked)

		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, store, openBeads, agent, "", "", "", true)
		if err != nil || !cleaned {
			t.Fatalf("cleanStaleHookedBeads = (%v, %v), want (true, nil)", cleaned, err)
		}
		if got, _ := store.Show(is.ID); got.Status != string(beads.IssueStatusHooked) || got.Assignee != agent {
			t.Errorf("dry run changed the bead: status=%q assignee=%q", got.Status, got.Assignee)
		}
	})

	t.Run("nothing assigned", func(t *testing.T) {
		t.Parallel()
		cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, beadsfake.New(), openBeads, agent, "", "", "", false)
		if err != nil || cleaned {
			t.Errorf("cleanStaleHookedBeads = (%v, %v), want (false, nil)", cleaned, err)
		}
	})

	t.Run("sweeps the town database and returns its lookup failures", func(t *testing.T) {
		t.Parallel()
		const normalized = "gastown/p1"
		boom := errors.New("bd list: connection refused")
		townRoot := t.TempDir()
		const rigPath = "rig-beads"

		for _, tc := range []struct {
			name         string
			failAssignee string
			wantErr      bool
		}{
			{"exact id fails", agent, true},
			{"normalized id fails", normalized, true},
			{"town answers", "", false},
		} {
			town := beadsfake.New()
			seedAssigned(t, town, normalized, beads.IssueStatusHooked)
			store := brokenStore{Client: town}
			if tc.wantErr {
				store.listErr, store.failAssignee = boom, tc.failAssignee
			}
			openStub := func(string) beads.Client { return store }
			cleaned, err := cleanStaleHookedBeads(&cobra.Command{}, beadsfake.New(), openStub, agent, "", townRoot, rigPath, false)

			if tc.wantErr {
				if !errors.Is(err, boom) || cleaned {
					t.Errorf("%s: = (%v, %v), want (false, an error wrapping %v)", tc.name, cleaned, err, boom)
				}
				continue
			}
			if err != nil || !cleaned {
				t.Errorf("%s: = (%v, %v), want (true, nil)", tc.name, cleaned, err)
			}
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
		cleaned, err := cleanStaleHookedBeads(cmd, store, openBeads, agent, "", "", "", false)
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
