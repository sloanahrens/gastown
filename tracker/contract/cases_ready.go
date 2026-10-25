package contract

import (
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "ready is open, unblocked, durable, not infrastructure, not deferred", Run: readyBasics})
	register(Case{Name: "an open blocks link hides an issue until the blocker closes", Run: readyBlocker})
	register(Case{Name: "a blocked parent blocks its children transitively, an open parent does not", Run: readyParentChild})
	register(Case{Name: "a deferral hides an issue until its time and a deferred issue wakes by itself", Run: readyDeferral})
	register(Case{Name: "links of other types and external targets never block", Run: readyNonBlocking})
	register(Case{Name: "ready filters: rig, parent, unassigned, excluded labels and types", Run: readyFilters})
	register(Case{Name: "a molecule scope reaches its wisp steps in dependency order and not the root", Run: readyMolecule})
	register(Case{Name: "a conditional-blocks link blocks like blocks does", Run: readyConditionalBlocks})
	register(Case{Name: "inside a molecule only links among its own steps hold a step back", Run: readyMoleculeLocal})
	register(Case{Name: "the ready board leaves out merge requests, gates, molecule roots and rig beads", Run: readyExcludedKinds})
	register(Case{Name: "ready pages in priority order without gaps", Run: readyPaging})
	register(Case{Name: "blocked lists what is held and by whom", Run: blockedList})
	register(Case{Name: "children of many parents, any status, wisps included", Run: childrenOfMany})
}

func mustReady(t *testing.T, s store.Store, f api.ReadyFilter) api.IssuePage {
	t.Helper()
	page, err := s.Ready(ctx(), f)
	if err != nil {
		t.Fatalf("Ready(%+v): %v", f, err)
	}
	return page
}

func readyBasics(t *testing.T, s store.Store) {
	open := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open"})
	prog := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "in progress"})
	done := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "done"})
	later := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "later"})
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "agent", Type: "agent"})
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "wisp", Ephemeral: true})
	mustUpdate(t, s, prog.ID, api.UpdatePatch{Status: str("in_progress")}, api.Guard{})
	mustUpdate(t, s, later.ID, api.UpdatePatch{Status: str("deferred")}, api.Guard{})
	if err := s.CloseIssues(ctx(), []string{done.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "ready", ids(mustReady(t, s, api.ReadyFilter{})), open.ID)
}

func readyBlocker(t *testing.T, s store.Store) {
	blocker := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "blocker"})
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "work"})
	mustDep(t, s, work.ID, blocker.ID, "blocks")
	sameSet(t, "ready while blocked", ids(mustReady(t, s, api.ReadyFilter{})), blocker.ID)
	if err := s.CloseIssues(ctx(), []string{blocker.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "ready after the blocker closed", ids(mustReady(t, s, api.ReadyFilter{})), work.ID)
	if err := s.DeleteIssues(ctx(), []string{work.ID}); err != nil {
		t.Fatal(err)
	}
	other := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "other"})
	gone := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "gone"})
	mustDep(t, s, other.ID, gone.ID, "blocks")
	if err := s.DeleteIssues(ctx(), []string{gone.ID}); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "ready after the blocker was deleted", ids(mustReady(t, s, api.ReadyFilter{})), other.ID)
}

func readyParentChild(t *testing.T, s store.Store) {
	gate := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "gate"})
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "parent"})
	child := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "child"})
	grand := mustCreate(t, s, api.CreateParams{Parent: child.ID, Title: "grandchild"})
	sameSet(t, "an open parent does not block", ids(mustReady(t, s, api.ReadyFilter{})), gate.ID, parent.ID, child.ID, grand.ID)
	mustDep(t, s, parent.ID, gate.ID, "blocks")
	sameSet(t, "a blocked parent blocks the whole subtree", ids(mustReady(t, s, api.ReadyFilter{})), gate.ID)
	if err := s.CloseIssues(ctx(), []string{gate.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "after the gate closed", ids(mustReady(t, s, api.ReadyFilter{})), parent.ID, child.ID, grand.ID)
}

func readyNonBlocking(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	c := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "c"})
	mustDep(t, s, a.ID, b.ID, "tracks")
	mustDep(t, s, a.ID, c.ID, "relates-to")
	mustDep(t, s, b.ID, "external:om:om-7", "blocks")
	sameSet(t, "ready", ids(mustReady(t, s, api.ReadyFilter{})), a.ID, b.ID, c.ID)
}

func readyFilters(t *testing.T, s store.Store) {
	parent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "parent"})
	kid := mustCreate(t, s, api.CreateParams{Parent: parent.ID, Title: "kid", Labels: []string{"gt:agent"}})
	mine := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "mine", Assignee: "me"})
	bug := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "bug", Type: "bug"})
	hq := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq"})
	sameSet(t, "rig hq", ids(mustReady(t, s, api.ReadyFilter{Rig: "hq"})), hq.ID)
	sameSet(t, "parent", ids(mustReady(t, s, api.ReadyFilter{Parent: parent.ID})), kid.ID)
	sameSet(t, "unassigned", ids(mustReady(t, s, api.ReadyFilter{Unassigned: true})), parent.ID, kid.ID, bug.ID, hq.ID)
	sameSet(t, "exclude label", ids(mustReady(t, s, api.ReadyFilter{ExcludeLabels: []string{"gt:agent", "x"}})), parent.ID, mine.ID, bug.ID, hq.ID)
	sameSet(t, "exclude type", ids(mustReady(t, s, api.ReadyFilter{ExcludeTypes: []string{"bug"}})), parent.ID, kid.ID, mine.ID, hq.ID)
}

func readyMolecule(t *testing.T, s store.Store) {
	root := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "mol", Type: "molecule", Ephemeral: true})
	step1 := mustCreate(t, s, api.CreateParams{Parent: root.ID, Title: "step 1", Ephemeral: true})
	step2 := mustCreate(t, s, api.CreateParams{Parent: root.ID, Title: "step 2", Ephemeral: true})
	sub := mustCreate(t, s, api.CreateParams{Parent: step1.ID, Title: "sub of step 1", Ephemeral: true})
	mustDep(t, s, step2.ID, step1.ID, "blocks")
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "unrelated"})
	sameSet(t, "scoped ready", ids(mustReady(t, s, api.ReadyFilter{Molecule: root.ID})), step1.ID, sub.ID)
	if err := s.CloseIssues(ctx(), []string{sub.ID, step1.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "after step 1 closed", ids(mustReady(t, s, api.ReadyFilter{Molecule: root.ID})), step2.ID)
	sameSet(t, "a durable board never shows wisps", ids(mustReady(t, s, api.ReadyFilter{})), mustReady(t, s, api.ReadyFilter{}).Issues[0].ID)
}

func readyPaging(t *testing.T, s store.Store) {
	prios := []int{2, 1, 2, 0, 1}
	made := make([]api.Issue, len(prios))
	for i, p := range prios {
		made[i] = mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "n", Priority: p})
	}
	var want []string
	for _, i := range []int{3, 1, 4, 0, 2} {
		want = append(want, made[i].ID)
	}
	var got []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		page := mustReady(t, s, api.ReadyFilter{Limit: 2, Cursor: cursor})
		got = append(got, ids(page)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged ready = %v, want %v", got, want)
	}
}

func blockedList(t *testing.T, s store.Store) {
	b1 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b1"})
	b2 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b2"})
	held := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held"})
	kid := mustCreate(t, s, api.CreateParams{Parent: held.ID, Title: "kid of held"})
	free := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "free"})
	mustDep(t, s, held.ID, b1.ID, "blocks")
	mustDep(t, s, held.ID, b2.ID, "blocks")
	page, err := s.Blocked(ctx(), api.ReadyFilter{})
	if err != nil {
		t.Fatal(err)
	}
	sameSet(t, "blocked", ids(page), held.ID, kid.ID)
	for _, is := range page.Issues {
		switch is.ID {
		case held.ID:
			sameSet(t, "held BlockedBy", is.BlockedBy, b1.ID, b2.ID)
		case kid.ID:
			if len(is.BlockedBy) != 0 {
				t.Errorf("the child is blocked through its parent, not directly: BlockedBy = %v", is.BlockedBy)
			}
		}
	}
	if err := s.CloseIssues(ctx(), []string{b1.ID, b2.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	if page, _ := s.Blocked(ctx(), api.ReadyFilter{}); len(page.Issues) != 0 {
		t.Errorf("blocked after the blockers closed = %v, want none", ids(page))
	}
	_ = free
}

func childrenOfMany(t *testing.T, s store.Store) {
	p1 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "p1"})
	p2 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "p2"})
	p3 := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "p3 childless"})
	a := mustCreate(t, s, api.CreateParams{Parent: p1.ID, Title: "a", Labels: []string{"x"}})
	b := mustCreate(t, s, api.CreateParams{Parent: p1.ID, Title: "b", Ephemeral: true})
	c := mustCreate(t, s, api.CreateParams{Parent: p2.ID, Title: "c"})
	if err := s.CloseIssues(ctx(), []string{c.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.ChildrenOf(ctx(), []string{p1.ID, p2.ID, p3.ID, "gt-nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ChildrenOf keys = %d, want only the two parents that have children", len(got))
	}
	if kids := got[p1.ID]; len(kids) != 2 || kids[0].ID != a.ID || kids[1].ID != b.ID {
		t.Errorf("children of p1 = %v, want [%s %s] in creation order, the wisp included", ids(api.IssuePage{Issues: kids}), a.ID, b.ID)
	} else if !slices.Equal(kids[0].Labels, []string{"x"}) {
		t.Errorf("child labels = %v, want [x]", kids[0].Labels)
	}
	if kids := got[p2.ID]; len(kids) != 1 || kids[0].ID != c.ID || kids[0].Status != "closed" {
		t.Errorf("children of p2 = %+v, want the closed child", kids)
	}
	if got, err := s.ChildrenOf(ctx(), nil); err != nil || len(got) != 0 {
		t.Errorf("ChildrenOf(nil) = %v, %v; want an empty map", got, err)
	}
}

func readyDeferral(t *testing.T, s store.Store) {
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	later := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "deferred into the future"})
	woken := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "deferred, time passed"})
	parked := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "deferred, no time"})
	openLater := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open but not yet"})
	cleared := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "deferral cleared"})
	mustUpdate(t, s, later.ID, api.UpdatePatch{Status: str("deferred"), DeferUntil: &future}, api.Guard{})
	mustUpdate(t, s, woken.ID, api.UpdatePatch{Status: str("deferred"), DeferUntil: &past}, api.Guard{})
	mustUpdate(t, s, parked.ID, api.UpdatePatch{Status: str("deferred")}, api.Guard{})
	mustUpdate(t, s, openLater.ID, api.UpdatePatch{DeferUntil: &future}, api.Guard{})
	mustUpdate(t, s, cleared.ID, api.UpdatePatch{DeferUntil: &future}, api.Guard{})
	mustUpdate(t, s, cleared.ID, api.UpdatePatch{DeferUntil: &time.Time{}}, api.Guard{})
	sameSet(t, "ready", ids(mustReady(t, s, api.ReadyFilter{})), woken.ID, cleared.ID)
	if got := mustGet(t, s, later.ID); got.DeferUntil == nil || got.DeferUntil.Before(time.Now()) {
		t.Errorf("DeferUntil = %v, want the future time that was set", got.DeferUntil)
	}
	if got := mustGet(t, s, cleared.ID); got.DeferUntil != nil {
		t.Errorf("DeferUntil = %v after being cleared, want none", got.DeferUntil)
	}
}

func readyConditionalBlocks(t *testing.T, s store.Store) {
	gate := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "gate"})
	held := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held by a conditional link"})
	mustDep(t, s, held.ID, gate.ID, "conditional-blocks")
	sameSet(t, "ready while the conditional blocker is open", ids(mustReady(t, s, api.ReadyFilter{})), gate.ID)
	if err := s.CloseIssues(ctx(), []string{gate.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "ready after it closed", ids(mustReady(t, s, api.ReadyFilter{})), held.ID)
}

// bd ready --mol answers from the molecule's own steps: a root that waits on
// its work bead, or a step that waits on something outside, does not hold the
// steps back, which is what gt's step walker needs to do the work in order.
func readyMoleculeLocal(t *testing.T, s store.Store) {
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "the work bead, still open"})
	root := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "mol", Type: "molecule", Ephemeral: true})
	one := mustCreate(t, s, api.CreateParams{Parent: root.ID, Title: "step one", Ephemeral: true})
	two := mustCreate(t, s, api.CreateParams{Parent: root.ID, Title: "step two", Ephemeral: true})
	mustDep(t, s, root.ID, work.ID, "blocks") // the root waits on an open bead outside
	mustDep(t, s, two.ID, one.ID, "blocks")   // a chain inside
	mustDep(t, s, one.ID, work.ID, "blocks")  // and step one also names the outside bead
	sub := mustCreate(t, s, api.CreateParams{Parent: two.ID, Title: "sub-step of step two", Ephemeral: true})
	// Step two is held by step one, and a blocked step holds back its own children.
	sameSet(t, "steps ready while the work bead is open", ids(mustReady(t, s, api.ReadyFilter{Molecule: root.ID})), one.ID)
	if err := s.CloseIssues(ctx(), []string{one.ID}, "r", true); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "steps ready after step one closed", ids(mustReady(t, s, api.ReadyFilter{Molecule: root.ID})), two.ID, sub.ID)
}

func readyExcludedKinds(t *testing.T, s store.Store) {
	work := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "work"})
	epic := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "an epic", Type: "epic"})
	feature := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a feature", Type: "feature"})
	for _, typ := range []string{"merge-request", "gate", "molecule", "rig", "agent", "role", "message"} {
		mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a " + typ, Type: typ})
	}
	sameSet(t, "ready", ids(mustReady(t, s, api.ReadyFilter{})), work.ID, epic.ID, feature.ID)
	// A molecule's own steps are still its steps, whatever their type.
	root := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "mol", Type: "molecule", Ephemeral: true})
	step := mustCreate(t, s, api.CreateParams{Parent: root.ID, Title: "a step that is a gate", Type: "gate", Ephemeral: true})
	sameSet(t, "steps of a molecule", ids(mustReady(t, s, api.ReadyFilter{Molecule: root.ID})), step.ID)
}
