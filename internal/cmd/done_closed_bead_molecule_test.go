package cmd

import (
	"reflect"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestDoneClosesMoleculeOfBeadClosedBeforeDone covers the orphan path
// gt-mddzp measured: the nothing-to-implement route closes the work bead
// itself (`gt bead close <id> --reason="no-changes: ..."`) and no landing
// follows it, so gt done's close block — the one place that ended an attached
// molecule — was skipped for the already-terminal bead. The molecule root and
// its step wisps stayed open for good (three of the four orphans measured).
func TestDoneClosesMoleculeOfBeadClosedBeforeDone(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead(),
		beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusClosed),
			CloseReason: "no-changes: the fix already landed",
			Description: "attached_molecule: gt-wisp-xyz"})
	db.Seed(beads.Issue{ID: "gt-wisp-xyz", Title: "mol-polecat-work", Status: "open", Ephemeral: true})
	seedChildren(t, db, "gt-wisp-xyz",
		beads.Issue{ID: "gt-step-1", Title: "Step 1", Status: "open", Ephemeral: true},
		beads.Issue{ID: "gt-step-2", Title: "Step 2", Status: "open", Ephemeral: true})

	rec := runDoneStateUpdate(t, db)

	got := rec.closed()
	sort.Strings(got)
	want := []string{"gt-step-1", "gt-step-2", "gt-wisp-xyz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closed = %v, want the molecule's steps and root %v", got, want)
	}
	for _, id := range append([]string{"gt-wisp-xyz"}, want[:2]...) {
		if is, err := db.Show(id); err != nil || is.Status != "closed" {
			t.Errorf("%s = %+v, %v; want closed", id, is, err)
		}
	}
}

// TestDoneClosesMoleculeOfBeadClosedBeforeDoneSkipsClosedSteps: a step already
// closed is not closed again, and the root still closes after it.
func TestDoneClosesMoleculeOfBeadClosedBeforeDoneSkipsClosedSteps(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead(),
		beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusClosed),
			Description: "attached_molecule: gt-wisp-xyz"})
	db.Seed(beads.Issue{ID: "gt-wisp-xyz", Title: "mol-polecat-work", Status: "open", Ephemeral: true})
	seedChildren(t, db, "gt-wisp-xyz",
		beads.Issue{ID: "gt-step-open", Title: "Step Open", Status: "open", Ephemeral: true},
		beads.Issue{ID: "gt-step-closed", Title: "Step Closed", Status: "closed", Ephemeral: true})

	rec := runDoneStateUpdate(t, db)

	if got, want := rec.closed(), []string{"gt-step-open", "gt-wisp-xyz"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closed = %v, want %v (the closed step is not closed again)", got, want)
	}
}

// TestDoneClosedBeadWithFinishedMoleculeIsQuiet: a molecule another path
// already ended (a landing, a burn) is not closed again, so a repeat gt done
// on the same bead leaves no warning behind.
func TestDoneClosedBeadWithFinishedMoleculeIsQuiet(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead(),
		beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusClosed),
			CloseReason: "no-changes: already fixed",
			Description: "attached_molecule: gt-wisp-xyz"})
	db.Seed(beads.Issue{ID: "gt-wisp-xyz", Title: "mol-polecat-work", Status: "closed", Ephemeral: true})

	rec := runDoneStateUpdate(t, db)

	if got := rec.closed(); len(got) != 0 {
		t.Fatalf("closed = %v, want none", got)
	}
}

// TestDoneClosedBeadWithoutMoleculeClosesNothing: a closed bead that carries no
// attachment is left alone — the close block's old behavior, kept.
func TestDoneClosedBeadWithoutMoleculeClosesNothing(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead(), beads.Issue{ID: "gt-base-123", Title: "Base bead",
		Status: string(beads.StatusClosed), Description: "no molecule attached"})

	rec := runDoneStateUpdate(t, db)

	if got := rec.closed(); len(got) != 0 {
		t.Fatalf("closed = %v, want none", got)
	}
}
