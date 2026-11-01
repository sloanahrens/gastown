package cmd

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// descendantsReadCounter counts the child reads a tree walk makes through a
// store, and refuses a read naming an id in refuse the way bd refuses a show.
type descendantsReadCounter struct {
	beads.Client
	children, childrenOf int
	parents              []string // each parent a read named, in call order
	refuse               map[string]bool
}

func (c *descendantsReadCounter) Children(parentID string) ([]*beads.Issue, error) {
	c.children++
	c.parents = append(c.parents, parentID)
	if c.refuse[parentID] {
		return nil, fmt.Errorf("children of %s refused", parentID)
	}
	return c.Client.Children(parentID)
}

func (c *descendantsReadCounter) ChildrenOf(parentIDs ...string) (map[string][]*beads.Issue, error) {
	c.childrenOf++
	c.parents = append(c.parents, parentIDs...)
	for _, id := range parentIDs {
		if c.refuse[id] {
			return nil, fmt.Errorf("children of %s refused", id)
		}
	}
	return c.Client.ChildrenOf(parentIDs...)
}

// total is the read calls the walk made, one per Children or ChildrenOf call.
func (c *descendantsReadCounter) total() int { return c.children + c.childrenOf }

// readsOf is how many read calls named parent.
func (c *descendantsReadCounter) readsOf(parent string) int {
	n := 0
	for _, id := range c.parents {
		if id == parent {
			n++
		}
	}
	return n
}

// childOf creates a step under parent and returns it.
func childOf(t *testing.T, db *beadsfake.Fake, title, parent string) *beads.Issue {
	t.Helper()
	return mustCreateBead(t, db, beads.CreateOptions{Title: title, Parent: parent})
}

// TestCloseDescendantsReadsOneCallPerLevel: a molecule is read one level at a
// time, one ChildrenOf per level, so the reads a burn makes grow with the
// depth of the tree and not with its size (gt-p9m39). The per-node Children
// recursion made 13 reads for the flat molecule.
func TestCloseDescendantsReadsOneCallPerLevel(t *testing.T) {
	t.Parallel()

	t.Run("flat molecule", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		root := mustCreateBead(t, db, beads.CreateOptions{Title: "mol-polecat-work"})
		var leaves []string
		for i := 0; i < 12; i++ {
			leaves = append(leaves, childOf(t, db, fmt.Sprintf("step %d", i), root.ID).ID)
		}
		counter := &descendantsReadCounter{Client: db}

		n, err := forceCloseDescendants(counter, root.ID)
		if err != nil {
			t.Fatalf("forceCloseDescendants: %v", err)
		}
		if n != len(leaves) {
			t.Errorf("closed %d, want %d steps", n, len(leaves))
		}
		if counter.total() > 2 {
			t.Errorf("%d child reads (%d Children, %d ChildrenOf) for the root and one level; want at most 2, one per level",
				counter.total(), counter.children, counter.childrenOf)
		}
		wantStatus(t, db, leaves, string(beads.StatusClosed))
	})

	t.Run("two-level molecule", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		root := mustCreateBead(t, db, beads.CreateOptions{Title: "mol-polecat-work"})
		var closed []string
		for i := 0; i < 3; i++ {
			step := childOf(t, db, fmt.Sprintf("step %d", i), root.ID)
			closed = append(closed, step.ID)
			for j := 0; j < 2; j++ {
				closed = append(closed, childOf(t, db, fmt.Sprintf("sub-step %d.%d", i, j), step.ID).ID)
			}
		}
		counter := &descendantsReadCounter{Client: db}

		n, err := forceCloseDescendants(counter, root.ID)
		if err != nil {
			t.Fatalf("forceCloseDescendants: %v", err)
		}
		if n != len(closed) {
			t.Errorf("closed %d, want %d steps", n, len(closed))
		}
		if counter.total() > 3 {
			t.Errorf("%d child reads (%d Children, %d ChildrenOf) for a root and two levels; want at most 3, one per level",
				counter.total(), counter.children, counter.childrenOf)
		}
		wantStatus(t, db, closed, string(beads.StatusClosed))
	})
}

// wantStatus checks each id's status in db.
func wantStatus(t *testing.T, db *beadsfake.Fake, ids []string, want string) {
	t.Helper()
	for _, id := range ids {
		got, err := db.Show(id)
		if err != nil {
			t.Fatalf("show %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("%s status %q, want %s", id, got.Status, want)
		}
	}
}

// closeCall is one close batch a walk made, in the order it made it.
type closeCall struct {
	force bool
	ids   []string
}

// closeBatchRecorder records every Close and ForceCloseWithReason batch.
type closeBatchRecorder struct {
	beads.Client
	calls []closeCall
}

func (c *closeBatchRecorder) Close(ids ...string) error {
	c.calls = append(c.calls, closeCall{ids: append([]string(nil), ids...)})
	return c.Client.Close(ids...)
}

func (c *closeBatchRecorder) ForceCloseWithReason(reason string, ids ...string) error {
	c.calls = append(c.calls, closeCall{force: true, ids: append([]string(nil), ids...)})
	return c.Client.ForceCloseWithReason(reason, ids...)
}

// descendantsResults is the tree the results test walks: a root with one step
// per outcome the sweep has to keep apart.
type descendantsResults struct {
	db         *beadsfake.Fake
	root       string
	closedStep string // closed, with an open sub-step below it
	subStep    string
	refused    string // open, assigned to another agent, so bd refuses it
	pinned     string // holding the hand-off pin
	open       string
}

func newDescendantsResults(t *testing.T) descendantsResults {
	t.Helper()
	db := beadsfake.New()
	r := descendantsResults{db: db, root: mustCreateBead(t, db, beads.CreateOptions{Title: "mol-polecat-work"}).ID}
	r.closedStep = childOf(t, db, "already closed", r.root).ID
	r.refused = childOf(t, db, "refused", r.root).ID
	r.pinned = childOf(t, db, "pinned", r.root).ID
	r.open = childOf(t, db, "open", r.root).ID

	// Closed before its sub-step was poured: the sweep must still descend
	// into a closed step, since no open step may survive under one (gt-7lx3).
	if err := db.Close(r.closedStep); err != nil {
		t.Fatalf("close %s: %v", r.closedStep, err)
	}
	r.subStep = childOf(t, db, "open under a closed step", r.closedStep).ID

	heldBy := "gastown/polecats/emerald"
	if err := db.Update(r.refused, beads.UpdateOptions{Assignee: &heldBy}); err != nil {
		t.Fatalf("assign %s: %v", r.refused, err)
	}
	pinStep(t, db, r.pinned)
	return r
}

// TestCloseDescendantsKeepsItsResults: only the reads change. The returned
// count, the batches handed to Close per parent, the refused step's
// PartialCloseError, and the open sub-step under an already-closed step all
// stay what the recursive walk produced (gt-p9m39, gt-7lx3).
func TestCloseDescendantsKeepsItsResults(t *testing.T) {
	t.Parallel()

	t.Run("unforced", func(t *testing.T) {
		t.Parallel()
		r := newDescendantsResults(t)
		rec := &closeBatchRecorder{Client: r.db}

		n, err := closeDescendantsImpl(rec, r.root, false)
		if n != 3 {
			t.Errorf("closed %d, want 3: the sub-step, the pinned step and the open step; the refused one stays open", n)
		}
		want := []closeCall{
			{ids: []string{r.subStep}},
			{ids: []string{r.refused, r.pinned, r.open}},
			{ids: []string{r.refused}},
		}
		if !slices.EqualFunc(rec.calls, want, equalCloseCall) {
			t.Errorf("close batches = %v, want %v", rec.calls, want)
		}
		var pe *beads.PartialCloseError
		if !errors.As(err, &pe) {
			t.Fatalf("error = %v, want a *beads.PartialCloseError naming the refused step", err)
		}
		if !slices.Equal(pe.NotClosed, []string{r.refused}) {
			t.Errorf("NotClosed = %v, want only the refused step %s", pe.NotClosed, r.refused)
		}
		wantStatus(t, r.db, []string{r.closedStep, r.subStep, r.pinned, r.open}, string(beads.StatusClosed))
		wantStatus(t, r.db, []string{r.refused}, string(beads.StatusOpen))
	})

	t.Run("forced", func(t *testing.T) {
		t.Parallel()
		r := newDescendantsResults(t)
		rec := &closeBatchRecorder{Client: r.db}

		n, err := forceCloseDescendants(rec, r.root)
		if err != nil {
			t.Fatalf("forceCloseDescendants: %v", err)
		}
		if n != 4 {
			t.Errorf("closed %d, want 4: the sub-step, the refused step, the pinned step and the open step", n)
		}
		want := []closeCall{
			{force: true, ids: []string{r.subStep}},
			{force: true, ids: []string{r.refused, r.pinned, r.open}},
		}
		if !slices.EqualFunc(rec.calls, want, equalCloseCall) {
			t.Errorf("force-close batches = %v, want %v", rec.calls, want)
		}
		wantStatus(t, r.db, []string{r.closedStep, r.subStep, r.refused, r.pinned, r.open}, string(beads.StatusClosed))
	})
}

// equalCloseCall reports whether two recorded batches are the same call. A
// batch is a set of ids, so their order inside it is not compared.
func equalCloseCall(a, b closeCall) bool {
	if a.force != b.force {
		return false
	}
	x, y := slices.Clone(a.ids), slices.Clone(b.ids)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
