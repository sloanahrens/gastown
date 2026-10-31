package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// childrenReadCounter counts the two reads specChildrenRead makes, so a test can
// hold the read to its bd calls.
type childrenReadCounter struct {
	beads.Client
	childrenOf, showMultiple int
}

func (c *childrenReadCounter) ChildrenOf(ids ...string) (map[string][]*beads.Issue, error) {
	c.childrenOf++
	return c.Client.ChildrenOf(ids...)
}

func (c *childrenReadCounter) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	c.showMultiple++
	return c.Client.ShowMultiple(ids)
}

// refusedShowClient refuses the second read, the one that tells a childless id
// from an unresolved one.
type refusedShowClient struct {
	beads.Client
}

func (refusedShowClient) ShowMultiple([]string) (map[string]*beads.Issue, error) {
	return nil, errors.New("show refused")
}

// refusedChildrenClient refuses the first read, the grouped children read.
type refusedChildrenClient struct {
	beads.Client
}

func (refusedChildrenClient) ChildrenOf(...string) (map[string][]*beads.Issue, error) {
	return nil, errors.New("children refused")
}

// specChildrenFixture holds fifty childless parents, one parent with a child,
// and an id that does not exist, in the order the candidates would list them.
func specChildrenFixture(t *testing.T) (db *beadsfake.Fake, ids []string, parent, child, missing string) {
	t.Helper()
	db = beadsfake.New()
	create := func(opts beads.CreateOptions) *beads.Issue {
		t.Helper()
		is, err := db.Create(opts)
		if err != nil {
			t.Fatalf("create %q: %v", opts.Title, err)
		}
		return is
	}
	for i := 0; i < 50; i++ {
		ids = append(ids, create(beads.CreateOptions{Title: fmt.Sprintf("childless %d", i), Priority: -1}).ID)
	}
	p := create(beads.CreateOptions{Title: "parent", Priority: -1})
	c := create(beads.CreateOptions{Title: "child", Parent: p.ID, Priority: -1})
	ids = append(ids, p.ID, "gt-missing")
	return db, ids, p.ID, c.ID, "gt-missing"
}

// Fifty childless parents cost one ChildrenOf and one ShowMultiple, not one
// read each: they exist, so each is keyed with an empty list. The parent with a
// child keeps its child, and the id that does not exist has no key, so the scan
// reads it alone (gt-4u2gt).
func TestSpecChildrenReadChildlessParentsCostTwoReads(t *testing.T) {
	t.Parallel()
	db, ids, parent, child, missing := specChildrenFixture(t)
	client := &childrenReadCounter{Client: db}

	got, err := specChildrenRead(client, ids)
	if err != nil {
		t.Fatalf("specChildrenRead: %v", err)
	}
	if client.childrenOf != 1 || client.showMultiple != 1 {
		t.Errorf("reads = %d ChildrenOf, %d ShowMultiple; want one of each", client.childrenOf, client.showMultiple)
	}
	for _, id := range ids[:50] {
		if kids, ok := got[id]; !ok || len(kids) != 0 {
			t.Errorf("childless %s: keyed %v, present %v; want keyed with no children", id, kids, ok)
		}
	}
	if kids := got[parent]; len(kids) != 1 || kids[0].ID != child {
		t.Errorf("parent children = %v; want the child %s", kids, child)
	}
	if _, ok := got[missing]; ok {
		t.Errorf("unresolved %s has a key; want none, so it is read alone", missing)
	}
}

// Fifty childless candidates of a rig cost one ChildrenOf and one ShowMultiple
// through the production default read: specCandidateChildren is given
// specChildrenReader, the function the tick's default read is, over a store that
// counts its calls. The children read that runs is specChildrenRead itself, so
// deleting its ShowMultiple step or reverting it to ChildrenOf turns this red
// (gt-4u2gt).
func TestSpecChildrenReadDefaultWiringCostsTwoReadsForChildless(t *testing.T) {
	t.Parallel()
	db, ids, _, _, _ := specChildrenFixture(t)
	client := &childrenReadCounter{Client: db}
	stores := slingStores{pinned: func(string) slingStore { return client }}

	cands := make([]specCandidate, 0, 50)
	for _, id := range ids[:50] {
		cands = append(cands, specCandidate{Spec: specdispatch.Spec{ID: id}})
	}
	reads := newSpecTickReads()
	children := specCandidateChildren(t.TempDir(), reads, cands, nil, specChildrenReader(stores))
	for _, c := range cands {
		if kids, err := children(c.Spec.ID); err != nil || len(kids) != 0 {
			t.Fatalf("children(%s) = %v, %v; want no children and no error", c.Spec.ID, kids, err)
		}
	}
	if reads.Calls() != 1 {
		t.Errorf("the tick made %d children reads for the rig; want one", reads.Calls())
	}
	if client.childrenOf != 1 || client.showMultiple != 1 {
		t.Errorf("fifty childless candidates cost %d ChildrenOf and %d ShowMultiple reads; want one of each", client.childrenOf, client.showMultiple)
	}
}

// A failed read is the whole group's error, so specCandidateChildren falls back
// to reading each bead alone and reports only the one that fails. Both reads
// are driven: the grouped children read, and the show that names the childless.
func TestSpecChildrenReadFailuresAreErrors(t *testing.T) {
	t.Parallel()
	db, ids, _, _, _ := specChildrenFixture(t)
	for _, tc := range []struct {
		name   string
		client beads.Client
	}{
		{"children read refused", refusedChildrenClient{Client: db}},
		{"show refused", refusedShowClient{Client: db}},
	} {
		if _, err := specChildrenRead(tc.client, ids); err == nil {
			t.Errorf("%s: specChildrenRead = nil error; want the failure", tc.name)
		}
	}
}
