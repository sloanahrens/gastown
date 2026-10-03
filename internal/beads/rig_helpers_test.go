package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// These tests run the rig-bead helpers over beadsfake, the way any Client runs
// them now that they are free functions over beads.Client (gt-7iwy0.4.10). The
// helpers are exported from package beads, so the tests live in the external
// test package to import the fake without an import cycle.

// TestRigBeadHelpersOverClient covers the read/create/update/delete/list round
// trip: the ID shape and gt:rig label a create lands, EnsureRigBead's
// idempotence, the parsed RigFields the getters return, and the delete.
func TestRigBeadHelpersOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	fields := &beads.RigFields{Repo: "git@example.com:gastown.git", Prefix: "gt", State: beads.RigStateActive}
	created, err := beads.CreateRigBead(c, "gastown", fields)
	if err != nil {
		t.Fatalf("CreateRigBead: %v", err)
	}
	if want := beads.RigBeadID("gastown"); created.ID != want {
		t.Fatalf("CreateRigBead ID = %q, want %q", created.ID, want)
	}
	if !beads.HasLabel(created, "gt:rig") {
		t.Fatalf("CreateRigBead labels = %v, want gt:rig", created.Labels)
	}

	// EnsureRigBead returns the bead that already exists, unchanged.
	ensured, err := beads.EnsureRigBead(c, "gastown", fields)
	if err != nil {
		t.Fatalf("EnsureRigBead: %v", err)
	}
	if ensured.ID != created.ID {
		t.Fatalf("EnsureRigBead ID = %q, want the existing %q", ensured.ID, created.ID)
	}

	// EnsureRigBead creates the bead when it is absent.
	if _, err := beads.EnsureRigBead(c, "beads", &beads.RigFields{Prefix: "be", State: beads.RigStateActive}); err != nil {
		t.Fatalf("EnsureRigBead (new rig): %v", err)
	}

	issue, got, err := beads.GetRigBead(c, "gastown")
	if err != nil {
		t.Fatalf("GetRigBead: %v", err)
	}
	if issue.ID != created.ID {
		t.Fatalf("GetRigBead ID = %q, want %q", issue.ID, created.ID)
	}
	if got == nil || got.Prefix != "gt" || got.Repo != fields.Repo || got.State != beads.RigStateActive {
		t.Fatalf("GetRigBead fields = %+v, want %+v", got, fields)
	}

	if _, byID, err := beads.GetRigByID(c, created.ID); err != nil || byID == nil || byID.Prefix != "gt" {
		t.Fatalf("GetRigByID = %+v, %v; want the gt rig", byID, err)
	}

	// Update rewrites the description field the state is parsed from.
	updated, err := beads.UpdateRigBead(c, "gastown", &beads.RigFields{Repo: fields.Repo, Prefix: "gt", State: beads.RigStateMaintenance})
	if err != nil {
		t.Fatalf("UpdateRigBead: %v", err)
	}
	if got := beads.ParseRigFields(updated.Description); got.State != beads.RigStateMaintenance {
		t.Fatalf("UpdateRigBead state = %q, want maintenance", got.State)
	}

	// List is keyed by prefix and covers every rig bead.
	rigs, err := beads.ListRigBeads(c)
	if err != nil {
		t.Fatalf("ListRigBeads: %v", err)
	}
	if len(rigs) != 2 || rigs["gt"] == nil || rigs["be"] == nil {
		t.Fatalf("ListRigBeads = %v, want gt and be", rigs)
	}

	if err := beads.DeleteRigBead(c, "gastown"); err != nil {
		t.Fatalf("DeleteRigBead: %v", err)
	}
	if _, _, err := beads.GetRigBead(c, "gastown"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("GetRigBead after delete = %v, want ErrNotFound", err)
	}
}

// TestGetRigBeadRejectsNonRigBead: a bead under the rig ID that is not a rig
// bead is an error, not a rig.
func TestGetRigBeadRejectsNonRigBead(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()
	if _, err := c.Create(beads.CreateOptions{ID: "gt-rig-gastown", Title: "gastown", Labels: []string{"gt:task"}}); err != nil {
		t.Fatalf("creating non-rig bead: %v", err)
	}
	_, _, err := beads.GetRigBead(c, "gastown")
	if err == nil || !strings.Contains(err.Error(), "not a rig bead") {
		t.Fatalf("GetRigBead on a non-rig bead = %v, want the missing-label error", err)
	}
}

// TestCreateRigBeadValidatesInput: the flag-like-name and invalid-state guards
// hold on the Client path exactly as they did on the *Beads method.
func TestCreateRigBeadValidatesInput(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	if _, err := beads.CreateRigBead(c, "--help", &beads.RigFields{Prefix: "gt"}); !errors.Is(err, beads.ErrFlagTitle) {
		t.Fatalf("CreateRigBead(--help) = %v, want ErrFlagTitle", err)
	}
	if _, err := beads.CreateRigBead(c, "gastown", &beads.RigFields{Prefix: "gt", State: "bogus"}); err == nil {
		t.Fatal("CreateRigBead with an invalid state should fail")
	}
}
