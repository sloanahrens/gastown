package beads_test

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// These run the force re-sling's guarded hand-back over beadsfake, the way any
// Client runs it now that TransferIfUnassigned is a free function over
// beads.Client (gt-ct2ob). The export is what puts them in the external test
// package: importing the fake from package beads would be an import cycle.

// TestTransferIfUnassignedOverClient covers the Client path's compare: a bead
// still open and unassigned is handed back, and one another actor has taken,
// or a closed one, is left alone.
func TestTransferIfUnassignedOverClient(t *testing.T) {
	t.Parallel()
	const holder = "gastown/polecats/Nux"
	hooked := beads.StatusHooked

	t.Run("open and unassigned is restored", func(t *testing.T) {
		t.Parallel()
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "unhooked work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		transferred, err := beads.TransferIfUnassigned(c, is.ID, hooked, holder)
		if err != nil || !transferred {
			t.Fatalf("TransferIfUnassigned = %v, %v; want true, nil", transferred, err)
		}
		if got, _ := c.Show(is.ID); got.Status != hooked || got.Assignee != holder {
			t.Errorf("bead ended %s to %q, want %s to %s", got.Status, got.Assignee, hooked, holder)
		}
	})

	t.Run("a bead another actor hooked is left with them", func(t *testing.T) {
		t.Parallel()
		thief := "gastown/polecats/Toast"
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "taken work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Update(is.ID, beads.UpdateOptions{Status: &hooked, Assignee: &thief}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		transferred, err := beads.TransferIfUnassigned(c, is.ID, hooked, holder)
		if err != nil || transferred {
			t.Errorf("TransferIfUnassigned over a hooked bead = %v, %v; want false, nil", transferred, err)
		}
		if got, _ := c.Show(is.ID); got.Status != hooked || got.Assignee != thief {
			t.Errorf("bead ended %s to %q, want %s's hold kept", got.Status, got.Assignee, thief)
		}
	})

	t.Run("a closed bead is not reopened", func(t *testing.T) {
		t.Parallel()
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "closed work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Close(is.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		transferred, err := beads.TransferIfUnassigned(c, is.ID, hooked, holder)
		if err != nil || transferred {
			t.Errorf("TransferIfUnassigned over a closed bead = %v, %v; want false, nil", transferred, err)
		}
		if got, _ := c.Show(is.ID); got.Status != string(beads.StatusClosed) || got.Assignee != "" {
			t.Errorf("bead ended %s to %q, want it left closed and unassigned", got.Status, got.Assignee)
		}
	})

	t.Run("a missing bead is an error", func(t *testing.T) {
		t.Parallel()
		transferred, err := beads.TransferIfUnassigned(beadsfake.New(), "gt-nosuch", hooked, holder)
		if transferred || !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("TransferIfUnassigned(missing) = %v, %v; want false, ErrNotFound", transferred, err)
		}
	})
}

// showFailsClient is a Client whose read fails, the way a bd lookup does when
// the database is unreachable.
type showFailsClient struct {
	beads.Client
	err error
}

func (c showFailsClient) Show(string) (*beads.Issue, error) { return nil, c.err }

// TestTransferIfHeldOverClient covers the compare a Client that is not *Beads
// does in two steps (gt-moxlb): the read holds off a bead whose status or
// assignee moved, and a failed read leaves the bead alone and surfaces the
// error. Each declined case leaves the bead exactly as it was.
func TestTransferIfHeldOverClient(t *testing.T) {
	t.Parallel()
	const holder = "gastown/crew/sloan"
	const thief = "gastown/polecats/Toast"
	hooked := beads.StatusHooked
	inProgress := string(beads.StatusInProgress)
	open := string(beads.StatusOpen)

	// held makes a bead held by assignee in status.
	held := func(t *testing.T, status, assignee string) (*beadsfake.Fake, string) {
		t.Helper()
		c := beadsfake.New()
		is, err := c.Create(beads.CreateOptions{Title: "held work", Priority: -1})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Update(is.ID, beads.UpdateOptions{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		return c, is.ID
	}

	t.Run("a bead still held in the status read is handed back", func(t *testing.T) {
		t.Parallel()
		c, id := held(t, hooked, holder)
		transferred, err := beads.TransferIfHeld(c, id, holder, hooked, open, "")
		if err != nil || !transferred {
			t.Fatalf("TransferIfHeld = %v, %v; want true, nil", transferred, err)
		}
		if got, _ := c.Show(id); got.Status != open || got.Assignee != "" {
			t.Errorf("bead ended %s to %q, want open and unassigned", got.Status, got.Assignee)
		}
	})

	for _, tc := range []struct {
		name         string
		status       string
		assignee     string
		wantStatus   string
		wantAssignee string
	}{
		{name: "a status mismatch leaves the bead alone", status: inProgress, assignee: holder,
			wantStatus: inProgress, wantAssignee: holder},
		{name: "an assignee mismatch leaves the bead alone", status: hooked, assignee: thief,
			wantStatus: hooked, wantAssignee: thief},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, id := held(t, tc.status, tc.assignee)
			transferred, err := beads.TransferIfHeld(c, id, holder, hooked, open, "")
			if err != nil || transferred {
				t.Errorf("TransferIfHeld = %v, %v; want false, nil", transferred, err)
			}
			if got, _ := c.Show(id); got.Status != tc.wantStatus || got.Assignee != tc.wantAssignee {
				t.Errorf("bead ended %s to %q, want %s to %q untouched", got.Status, got.Assignee, tc.wantStatus, tc.wantAssignee)
			}
		})
	}

	t.Run("a failed read leaves the bead alone and reports the error", func(t *testing.T) {
		t.Parallel()
		c, id := held(t, hooked, holder)
		readErr := errors.New("dolt unreachable")
		transferred, err := beads.TransferIfHeld(showFailsClient{Client: c, err: readErr}, id, holder, hooked, open, "")
		if transferred || !errors.Is(err, readErr) {
			t.Errorf("TransferIfHeld = %v, %v; want false, the read error", transferred, err)
		}
		if got, _ := c.Show(id); got.Status != hooked || got.Assignee != holder {
			t.Errorf("bead ended %s to %q, want %s's hold kept", got.Status, got.Assignee, holder)
		}
	})
}
