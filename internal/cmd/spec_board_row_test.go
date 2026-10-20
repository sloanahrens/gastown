package cmd

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A ready-board row carries the fields the lint and the held check read, so the
// spec a candidate is drawn with is the row's, not a second read (gt-bqj8q).
func TestSpecFromBoardRowCarriesTheSpec(t *testing.T) {
	t.Parallel()
	row := `[{"id":"gt-r1","title":"row title","issue_type":"task","status":"open","priority":1,` +
		`"created_at":"2026-10-20T10:00:00Z","description":"the description","notes":"a note",` +
		`"acceptance_criteria":"- [ ] it works","design":"the design","labels":["area:x"]}]`
	issues, err := parseSpecReady([]byte(row))
	if err != nil {
		t.Fatalf("parseSpecReady: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("parsed %d rows, want 1", len(issues))
	}
	got := specFromIssue(issues[0])
	want := specdispatch.Spec{
		ID: "gt-r1", Title: "row title", Type: "task", Status: "open", Priority: 1,
		CreatedAt: "2026-10-20T10:00:00Z", Labels: []string{"area:x"},
		Description: "the description", Design: "the design", Notes: "a note",
		Acceptance: "- [ ] it works",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec from board row = %+v, want %+v", got, want)
	}
}

// A row that omits the design (bd leaves an empty one off) is filled from one
// ShowMultiple per rig; a row that carries one is not read at all, and a
// candidate whose design cannot be read is dropped with its error (gt-bqj8q).
func TestSpecBoardDesignsReadsOncePerRig(t *testing.T) {
	t.Parallel()
	alphaDB := beadsfake.New()
	alphaDB.Seed(
		beads.Issue{ID: "gt-a1", Design: "alpha design"},
		beads.Issue{ID: "gt-a2", Design: "not taken: the row carries it"},
	)
	betaDB := beadsfake.New()
	betaDB.Seed(beads.Issue{ID: "gt-b1"})
	stores := map[string]*countingStore{
		"alpha": newCountingStore(alphaDB),
		"beta":  newCountingStore(betaDB),
	}
	storeFor := func(rig string) beads.Client { return stores[rig] }

	read := specBoardRead{Candidates: []specCandidate{
		{Spec: specdispatch.Spec{ID: "gt-a1", Title: "a1"}, Rig: "alpha"},
		{Spec: specdispatch.Spec{ID: "gt-a2", Design: "row design"}, Rig: "alpha"},
		{Spec: specdispatch.Spec{ID: "gt-b1"}, Rig: "beta"},
		{Spec: specdispatch.Spec{ID: "gt-b2"}, Rig: "beta"},
	}}
	reads := newSpecTickReads()
	got := specBoardDesigns(reads, storeFor, read)

	ids := make([]string, 0, len(got.Candidates))
	for _, c := range got.Candidates {
		ids = append(ids, c.Spec.ID)
	}
	if want := []string{"gt-a1", "gt-a2", "gt-b1"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("candidates = %v, want %v", ids, want)
	}
	if d := got.Candidates[0].Spec.Design; d != "alpha design" {
		t.Errorf("gt-a1 design = %q, want the rig's", d)
	}
	if d := got.Candidates[1].Spec.Design; d != "row design" {
		t.Errorf("gt-a2 design = %q, want the row's", d)
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "gt-b2") {
		t.Errorf("errors = %v, want gt-b2 named once", got.Errors)
	}
	if want := [][]string{{"gt-a1"}}; !reflect.DeepEqual(stores["alpha"].multis, want) {
		t.Errorf("alpha batches = %v, want %v", stores["alpha"].multis, want)
	}
	if want := [][]string{{"gt-b1", "gt-b2"}}; !reflect.DeepEqual(stores["beta"].multis, want) {
		t.Errorf("beta batches = %v, want %v", stores["beta"].multis, want)
	}
	for rig, s := range stores {
		if len(s.shows) != 0 {
			t.Errorf("%s: singly read %v, want none", rig, s.shows)
		}
	}
	if reads.Calls() != 2 {
		t.Errorf("tick cost counts %d calls, want one per rig", reads.Calls())
	}
}

// A failed batched read drops the candidates it was meant to fill, naming the
// read's own error, and leaves the candidates whose row carries a design alone.
// The dropped bead waits for the next tick (gt-bqj8q).
func TestSpecBoardDesignsDropsCandidatesWhenTheBatchFails(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-c1", Design: "c1 design"})
	store := newCountingStore(db)
	store.failBatch = true
	storeFor := func(string) beads.Client { return store }

	read := specBoardRead{Candidates: []specCandidate{
		{Spec: specdispatch.Spec{ID: "gt-c1"}, Rig: "alpha"},
		{Spec: specdispatch.Spec{ID: "gt-c2", Design: "row design"}, Rig: "alpha"},
	}}
	reads := newSpecTickReads()
	got := specBoardDesigns(reads, storeFor, read)

	ids := make([]string, 0, len(got.Candidates))
	for _, c := range got.Candidates {
		ids = append(ids, c.Spec.ID)
	}
	if want := []string{"gt-c2"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("candidates = %v, want %v", ids, want)
	}
	if len(got.Errors) != 1 || got.Errors[0] != "gt-c1: cannot read design: bd show: boom" {
		t.Errorf("errors = %v, want gt-c1 dropped with the batch's error", got.Errors)
	}
	if reads.Calls() != 1 {
		t.Errorf("tick cost counts %d calls, want the one batch", reads.Calls())
	}
}

// The candidate loop reads no bead however many candidates the board holds, so
// its reads do not grow with N. Each bead the tick slings still gets its
// last-moment re-read before the sling (gt-bqj8q).
func TestSpecTickCandidateLoopReadsNothing(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 40} {
		var specs []specdispatch.Spec
		for i := 0; i < n; i++ {
			specs = append(specs, cleanSpec(fmt.Sprintf("gt-n%02d", i), 2, "2026-09-29T10:00:00Z"))
		}
		f := newFakeSpecTown(specs...)
		env := f.env()
		env.ShapeGate = specdispatch.ShapeGateWarn
		show := env.Show
		candidateReads, freshReads := 0, 0
		env.Show = func(id string) (specdispatch.Spec, error) {
			candidateReads++
			return show(id)
		}
		env.ShowFresh = func(id string) (specdispatch.Spec, error) {
			freshReads++
			return show(id)
		}
		r := runSpecDispatchCycle(env)

		if len(r.Dispatched) == 0 {
			t.Fatalf("n=%d: nothing dispatched", n)
		}
		if candidateReads != 0 {
			t.Errorf("n=%d: candidate loop made %d reads, want none", n, candidateReads)
		}
		if freshReads != len(r.Dispatched) {
			t.Errorf("n=%d: %d pre-sling re-reads for %d slung beads", n, freshReads, len(r.Dispatched))
		}
	}
}

// parseSpecReady reads a bare array and an envelope alike. A zero
// dependent_count on a row no longer says anything about the bead's children
// (gt-603wu).
func TestSpecFromBoardRowReadsBothShapes(t *testing.T) {
	t.Parallel()
	rows := `[{"id":"gt-zero","issue_type":"task","status":"open","dependent_count":0}]`
	issues, err := parseSpecReady([]byte(rows))
	if err != nil || len(issues) != 1 || issues[0].ID != "gt-zero" {
		t.Fatalf("bare array = %v, %v; want the zero-count row", issues, err)
	}
	env := `{"issues":[{"id":"gt-env","issue_type":"task","status":"open"}]}`
	issues, err = parseSpecReady([]byte(env))
	if err != nil || len(issues) != 1 || issues[0].ID != "gt-env" {
		t.Fatalf("envelope = %v, %v; want one row", issues, err)
	}
}

// Every candidate gets the children read, whatever its board row counted: bd
// counts only blocks edges there, so a parent whose only children are
// parent-child edges carries a zero and must still be held on its open child
// (gt-603wu).
func TestSpecTickReadsChildrenForEveryCandidate(t *testing.T) {
	t.Parallel()
	childless := []string{"gt-k1", "gt-k2", "gt-k3"}
	var specs []specdispatch.Spec
	for _, id := range childless {
		specs = append(specs, cleanSpec(id, 2, "2026-09-29T10:00:00Z"))
	}
	specs = append(specs, cleanSpec("gt-parent", 2, "2026-09-29T10:00:00Z"))
	f := newFakeSpecTown(specs...)
	f.children["gt-parent"] = []specdispatch.Child{{ID: "gt-parent.1", Status: "open"}}
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.PerTick = 10
	children := env.Children
	childReads := map[string]int{}
	env.Children = func(id string) ([]specdispatch.Child, error) {
		childReads[id]++
		return children(id)
	}
	r := runSpecDispatchCycle(env)

	for _, id := range append(childless, "gt-parent") {
		if n := childReads[id]; n != 1 {
			t.Errorf("%s: children read %d times, want 1", id, n)
		}
	}
	dispatched := map[string]bool{}
	for _, d := range r.Dispatched {
		dispatched[d.Bead] = true
	}
	if dispatched["gt-parent"] {
		t.Error("gt-parent was slung past its open child")
	}
	for _, id := range childless {
		if !dispatched[id] {
			t.Errorf("%s was not slung, want it dispatched", id)
		}
	}
}
