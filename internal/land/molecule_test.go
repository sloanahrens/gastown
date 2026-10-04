package land

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

const (
	fixtureWorkBead = "gt-work"
	fixtureMolecule = "gt-wisp-mol"
	fixtureSteps    = 3
)

func stepID(n int) string { return "gt-wisp-step" + strconv.Itoa(n) }

// moleculeFixture is a work bead carrying an attached molecule: a root with
// three step wisps chained by blocks dependencies, as sling mints them.
type moleculeFixture struct {
	t  *testing.T
	bd *beadsfake.Fake
	l  *Lander
}

func newMoleculeFixture(t *testing.T) *moleculeFixture {
	t.Helper()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: fixtureWorkBead, Title: "the work", Status: "closed", Type: "task",
		Description: "attached_molecule: " + fixtureMolecule + "\nattached_formula: mol-polecat-work\n"})
	for i := 1; i <= fixtureSteps; i++ {
		bd.Seed(beads.Issue{ID: stepID(i), Title: "step " + strconv.Itoa(i), Status: "open", Type: "task"})
	}
	bd.Seed(beads.Issue{ID: fixtureMolecule, Title: "mol-polecat-work", Status: "open", Type: "molecule"})
	return &moleculeFixture{t: t, bd: bd, l: &Lander{Beads: bd, Out: &bytes.Buffer{}}}
}

// chain wires the molecule in the order given: each step is a child of the
// root and blocked by the step before it, and the root is blocked by the
// work bead. Passing a descending order makes the listing run the tail of
// the chain first, which no single bd close pass can close.
func (f *moleculeFixture) chain(order ...int) {
	f.t.Helper()
	for _, n := range order {
		if err := f.bd.AddTypedDependency(stepID(n), fixtureMolecule, "parent-child"); err != nil {
			f.t.Fatal(err)
		}
		if n > 1 {
			if err := f.bd.AddDependency(stepID(n), stepID(n-1)); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	if err := f.bd.AddDependency(fixtureMolecule, fixtureWorkBead); err != nil {
		f.t.Fatal(err)
	}
}

func (f *moleculeFixture) status(id string) string {
	f.t.Helper()
	is, err := f.bd.Show(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return is.Status
}

func (f *moleculeFixture) wantStatus(id, want string) {
	f.t.Helper()
	if got := f.status(id); got != want {
		f.t.Errorf("%s status = %q, want %q", id, got, want)
	}
}

func (f *moleculeFixture) log() string { return f.l.Out.(*bytes.Buffer).String() }

// TestLandCloseClosesTheAttachedMolecule: a closed work bead takes its whole
// molecule with it, steps and root, through an unforced close.
func TestLandCloseClosesTheAttachedMolecule(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)

	f.l.closeAttachedMolecule(fixtureWorkBead)

	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "closed")
}

// TestLandCloseClosesTheChainOutOfOrder: bd refuses a step whose blocker is
// open at its turn, so a listing that runs the tail of the chain first needs
// a second pass — the landing retries rather than reaching for --force.
func TestLandCloseClosesTheChainOutOfOrder(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(3, 2, 1)

	f.l.closeAttachedMolecule(fixtureWorkBead)

	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "closed")
}

// TestLandCloseLeavesTheMoleculeOfAnOpenWorkBead: the molecule is the
// polecat's workflow, and a work bead that has not closed keeps it.
func TestLandCloseLeavesTheMoleculeOfAnOpenWorkBead(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	if err := f.bd.Update(fixtureWorkBead, beads.UpdateOptions{Status: ptrTo("open")}); err != nil {
		t.Fatal(err)
	}

	f.l.closeAttachedMolecule(fixtureWorkBead)

	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "open")
	}
	f.wantStatus(fixtureMolecule, "open")
}

// TestLandCloseOnAClosedMoleculeIsANoOp: a repair pass over a landing that
// already closed the molecule changes nothing.
func TestLandCloseOnAClosedMoleculeIsANoOp(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	for i := 1; i <= fixtureSteps; i++ {
		if err := f.bd.ForceCloseWithReason("landed", stepID(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.bd.ForceCloseWithReason("landed", fixtureMolecule); err != nil {
		t.Fatal(err)
	}

	f.l.closeAttachedMolecule(fixtureWorkBead)

	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "closed")
	if out := f.log(); out != "" {
		t.Errorf("a no-op close logged %q", out)
	}
}

// TestLandCloseLeavesARefusedStepOpenWithoutForce: a step bd refuses (here,
// one held by another actor) stays open, and so does the root above it. The
// landing never falls back to --force.
func TestLandCloseLeavesARefusedStepOpenWithoutForce(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	if err := f.bd.Update(stepID(1), beads.UpdateOptions{Assignee: ptrTo("gastown/polecats/other")}); err != nil {
		t.Fatal(err)
	}

	f.l.closeAttachedMolecule(fixtureWorkBead)

	f.wantStatus(stepID(1), "open")
	f.wantStatus(fixtureMolecule, "open")
	if !strings.Contains(f.log(), "left open after landing") {
		t.Errorf("the refused molecule was not logged: %q", f.log())
	}
}

// TestLandClosesTheMoleculeThroughTheLanding: the wiring, not just the
// helper — recordBead closes the molecule of the bead it lands.
func TestLandClosesTheMoleculeThroughTheLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	seedMoleculeOn(t, f.bd)
	desc := "attached_molecule: " + fixtureMolecule + "\nattached_formula: mol-polecat-work\n"
	if err := f.bd.Update("gt-abc", beads.UpdateOptions{Description: &desc}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}

	if got := f.status("gt-abc"); got != "closed" {
		t.Errorf("work bead status = %q, want closed", got)
	}
	if got := f.status(fixtureMolecule); got != "closed" {
		t.Errorf("molecule status = %q, want closed", got)
	}
}

// seedMoleculeOn adds the fixture molecule and its step chain to bd.
func seedMoleculeOn(t *testing.T, bd *beadsfake.Fake) {
	t.Helper()
	bd.Seed(beads.Issue{ID: fixtureMolecule, Title: "mol-polecat-work", Status: "open", Type: "molecule"})
	for i := 1; i <= fixtureSteps; i++ {
		bd.Seed(beads.Issue{ID: stepID(i), Title: "step " + strconv.Itoa(i), Status: "open", Type: "task"})
		if err := bd.AddTypedDependency(stepID(i), fixtureMolecule, "parent-child"); err != nil {
			t.Fatal(err)
		}
		if i > 1 {
			if err := bd.AddDependency(stepID(i), stepID(i-1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := bd.AddDependency(fixtureMolecule, "gt-abc"); err != nil {
		t.Fatal(err)
	}
}

func (f *landFixture) status(id string) string {
	f.t.Helper()
	is, err := f.bd.Show(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return is.Status
}

func ptrTo[T any](v T) *T { return &v }
