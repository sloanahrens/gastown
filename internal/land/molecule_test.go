package land

import (
	"bytes"
	"context"
	"errors"
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

// TestCloseMolecule_ReleasesPinnedStep: a step left under the hand-off pin is
// not unfinished work, but bd refuses to close a pinned issue. The landing
// releases the pin before the close, so the step and the root above it close
// instead of the molecule staying open after landing (gt-o6kks).
func TestCloseMolecule_ReleasesPinnedStep(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	if err := f.bd.Update(stepID(2), beads.UpdateOptions{Status: ptrTo("pinned")}); err != nil {
		t.Fatal(err)
	}

	if _, err := closeMolecule(f.bd, fixtureMolecule); err != nil {
		t.Fatalf("closeMolecule: %v", err)
	}

	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "closed")
}

// callRecorder is a moleculeBeads that records the reads and writes a close
// makes, in order, so a test can pin the shape of one pass and not only its
// result.
type callRecorder struct {
	moleculeBeads
	calls []string
}

func (c *callRecorder) record(call string) { c.calls = append(c.calls, call) }

func (c *callRecorder) Show(id string) (*beads.Issue, error) {
	c.record("show " + id)
	return c.moleculeBeads.Show(id)
}

func (c *callRecorder) Children(parentID string) ([]*beads.Issue, error) {
	c.record("children " + parentID)
	return c.moleculeBeads.Children(parentID)
}

func (c *callRecorder) Update(id string, opts beads.UpdateOptions) error {
	c.record("update " + id)
	return c.moleculeBeads.Update(id, opts)
}

func (c *callRecorder) CloseWithReason(reason string, ids ...string) error {
	c.record("close " + strings.Join(ids, ","))
	return c.moleculeBeads.CloseWithReason(reason, ids...)
}

// matching returns the recorded calls with the given prefix.
func (c *callRecorder) matching(prefix string) []string {
	var out []string
	for _, call := range c.calls {
		if strings.HasPrefix(call, prefix) {
			out = append(out, call)
		}
	}
	return out
}

// TestCloseMoleculeClosedRoot: a molecule the landing already closed costs a
// read of the root and a read of its children, and no recursion into the
// tree, no write. A landing pass that runs again over the same bead walks
// nothing (gt-vf3hs8).
func TestCloseMoleculeClosedRoot(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	if _, err := closeMolecule(f.bd, fixtureMolecule); err != nil {
		t.Fatalf("the first close: %v", err)
	}
	rec := &callRecorder{moleculeBeads: f.bd}

	n, err := closeMolecule(rec, fixtureMolecule)

	if n != 0 || err != nil {
		t.Fatalf("closeMolecule on a closed molecule = %d, %v, want 0, nil", n, err)
	}
	if reads := rec.matching("show "); len(reads) != 1 || reads[0] != "show "+fixtureMolecule {
		t.Errorf("a closed molecule was read as %v, want one read of the root", reads)
	}
	if reads := rec.matching("children "); len(reads) != 1 || reads[0] != "children "+fixtureMolecule {
		t.Errorf("a closed molecule was walked as %v, want one children read of the root", reads)
	}
	if writes := append(rec.matching("update "), rec.matching("close ")...); len(writes) > 0 {
		t.Errorf("a closed molecule was written: %v", writes)
	}
}

// TestCloseMoleculeForcedClosedRootClosesItsSteps: a forced close of the root
// skips bd's open-children fence and leaves the steps open. The root is
// terminal, but the landing still closes the steps it finds, unpinning them
// first, and leaves the closed root alone (gt-vf3hs8).
func TestCloseMoleculeForcedClosedRootClosesItsSteps(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	if err := f.bd.Update(stepID(3), beads.UpdateOptions{Status: ptrTo("pinned")}); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.ForceCloseWithReason("reaped", fixtureMolecule); err != nil {
		t.Fatal(err)
	}

	n, err := closeMolecule(f.bd, fixtureMolecule)

	if err != nil {
		t.Fatalf("closeMolecule: %v", err)
	}
	if n != fixtureSteps {
		t.Errorf("closed %d bead(s), want %d", n, fixtureSteps)
	}
	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "closed")
}

// TestCloseMoleculeOpenRootReadOnceAndClosedLast: an open root is read once,
// its steps close deepest first, and the root closes last.
func TestCloseMoleculeOpenRootReadOnceAndClosedLast(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	// A tree, not the flat chain: step 2 is step 1's child, so the close
	// order has to run step 2 before step 1.
	for _, id := range []string{stepID(1), stepID(3)} {
		if err := f.bd.AddTypedDependency(id, fixtureMolecule, "parent-child"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.bd.AddTypedDependency(stepID(2), stepID(1), "parent-child"); err != nil {
		t.Fatal(err)
	}
	rec := &callRecorder{moleculeBeads: f.bd}

	n, err := closeMolecule(rec, fixtureMolecule)

	if err != nil {
		t.Fatalf("closeMolecule: %v", err)
	}
	if n != fixtureSteps+1 {
		t.Errorf("closed %d bead(s), want the %d steps and the root", n, fixtureSteps)
	}
	if reads := rec.matching("show "); len(reads) != 1 || reads[0] != "show "+fixtureMolecule {
		t.Errorf("the root was read as %v, want once", reads)
	}
	closes := rec.matching("close ")
	if len(closes) == 0 {
		t.Fatalf("no close was made: %v", rec.calls)
	}
	if last := closes[len(closes)-1]; last != "close "+fixtureMolecule {
		t.Errorf("the last close = %q, want the root alone; calls: %v", last, rec.calls)
	}
	steps := closes[0]
	deeper, shallower := strings.Index(steps, stepID(2)), strings.Index(steps, stepID(1))
	if deeper < 0 || shallower < 0 || deeper > shallower {
		t.Errorf("the first close = %q, want %s before its parent %s", steps, stepID(2), stepID(1))
	}
}

// TestCloseMoleculeMissingRootIsANoOp: a molecule bead the store does not
// hold is no work, not a failure, and its tree is never listed.
func TestCloseMoleculeMissingRootIsANoOp(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	rec := &callRecorder{moleculeBeads: f.bd}

	n, err := closeMolecule(rec, "gt-wisp-missing")

	if n != 0 || err != nil {
		t.Fatalf("closeMolecule on a missing root = %d, %v, want 0, nil", n, err)
	}
	if walks := rec.matching("children "); len(walks) > 0 {
		t.Errorf("a missing root was walked: %v", rec.calls)
	}
}

// TestCloseMoleculeUnreadableRootStillClosesTheSteps: a root read that fails
// for any reason but absence must not abandon the steps behind it. The walk
// still runs, the steps it can see are closed, and the read error is returned
// wrapped and joined with any close error.
func TestCloseMoleculeUnreadableRootStillClosesTheSteps(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	f.bd.FailWith("show", errors.New("bd show: store unavailable"))

	n, err := closeMolecule(f.bd, fixtureMolecule)
	f.bd.FailWith("show", nil)

	if err == nil || !strings.Contains(err.Error(), "reading molecule "+fixtureMolecule) {
		t.Fatalf("closeMolecule err = %v, want the root read error named", err)
	}
	if n != fixtureSteps {
		t.Errorf("closed %d bead(s), want the %d steps", n, fixtureSteps)
	}
	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "open")
}

// childrenFailingBeads is a moleculeBeads whose children read fails for one
// parent, as a bd read that could not answer would.
type childrenFailingBeads struct {
	moleculeBeads
	refused string
	err     error
}

func (c childrenFailingBeads) Children(parentID string) ([]*beads.Issue, error) {
	if parentID == c.refused {
		return nil, c.err
	}
	return c.moleculeBeads.Children(parentID)
}

// TestCloseMoleculeUnreadableChildren: a children read that fails, at the
// root or one level down, is returned named and nothing is closed, so a
// half-read tree is never closed blind.
func TestCloseMoleculeUnreadableChildren(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, refused string }{
		{"root", fixtureMolecule},
		{"step", stepID(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMoleculeFixture(t)
			f.chain(1, 2, 3)
			rec := &callRecorder{moleculeBeads: childrenFailingBeads{
				moleculeBeads: f.bd, refused: tc.refused, err: errors.New("bd children: store unavailable"),
			}}

			n, err := closeMolecule(rec, fixtureMolecule)

			if err == nil || !strings.Contains(err.Error(), "listing children of "+tc.refused) {
				t.Fatalf("closeMolecule err = %v, want the children read error named", err)
			}
			if n != 0 {
				t.Errorf("closed %d bead(s), want none", n)
			}
			if writes := rec.matching("close "); len(writes) > 0 {
				t.Errorf("a half-read tree was closed: %v", writes)
			}
			for _, id := range []string{stepID(1), stepID(2), stepID(3), fixtureMolecule} {
				f.wantStatus(id, "open")
			}
		})
	}
}

// unpinRefusingBeads is a moleculeBeads whose unpin write fails for one step,
// as a bd refusal of that write would.
type unpinRefusingBeads struct {
	moleculeBeads
	refused string
}

func (u unpinRefusingBeads) Update(id string, opts beads.UpdateOptions) error {
	if id == u.refused {
		return errors.New("bd update refused")
	}
	return u.moleculeBeads.Update(id, opts)
}

// TestCloseMolecule_OneFailedUnpinDoesNotStopTheOthers: a step whose unpin
// fails stays pinned, so bd refuses it and the molecule stays open, but the
// other pinned step is still released and closed, and the error names the
// refused step rather than aborting the whole close.
func TestCloseMolecule_OneFailedUnpinDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	f := newMoleculeFixture(t)
	f.chain(1, 2, 3)
	for _, id := range []string{"gt-wisp-refused", "gt-wisp-released"} {
		f.bd.Seed(beads.Issue{ID: id, Title: id, Status: "pinned", Type: "task"})
		if err := f.bd.AddTypedDependency(id, fixtureMolecule, "parent-child"); err != nil {
			t.Fatal(err)
		}
	}

	_, err := closeMolecule(unpinRefusingBeads{moleculeBeads: f.bd, refused: "gt-wisp-refused"}, fixtureMolecule)

	if err == nil || !strings.Contains(err.Error(), "gt-wisp-refused") {
		t.Fatalf("closeMolecule err = %v, want the refused unpin named", err)
	}
	f.wantStatus("gt-wisp-released", "closed")
	f.wantStatus("gt-wisp-refused", "pinned")
	for i := 1; i <= fixtureSteps; i++ {
		f.wantStatus(stepID(i), "closed")
	}
	f.wantStatus(fixtureMolecule, "open")
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
