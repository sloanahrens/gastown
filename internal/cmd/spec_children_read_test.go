package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	chunks := (len(cands) + specChildrenChunkSize - 1) / specChildrenChunkSize
	if reads.Calls() != chunks {
		t.Errorf("the tick made %d children reads for the rig; want %d, one per chunk", reads.Calls(), chunks)
	}
	if client.childrenOf != chunks || client.showMultiple != chunks {
		t.Errorf("fifty childless candidates cost %d ChildrenOf and %d ShowMultiple reads; want %d of each, one per chunk", client.childrenOf, client.showMultiple, chunks)
	}
}

// A chunk read the deadline ended is left for the next tick: one call, no
// per-bead call, and every id of the chunk reports the timeout. A failure that
// is not a timeout keeps the per-bead fallback, for that chunk's ids only
// (gt-qrz8rm). Mutation: dropping the isBDTimeout branch reads each id alone
// and reports the reader's own error.
func TestTickEffTimeoutDoesNotFallBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	read := specCandidateBoardRead(t, specCandidateFixture())
	cands := read.Candidates[:specChildrenChunkSize]
	for _, tc := range []struct {
		fail     error
		fallback bool
	}{
		{fmt.Errorf("bd show --children: %w", context.DeadlineExceeded), false},
		{fakeExit(124), false}, // the gateway's kill
		{errors.New("bd show --children: refused"), true},
	} {
		reads := newSpecTickReads()
		var singles []string
		children := specCandidateChildren(root, reads, cands, nil, func(_ string, ids []string) (map[string][]*beads.Issue, error) {
			if len(ids) > 1 {
				return nil, tc.fail
			}
			singles = append(singles, ids[0])
			return map[string][]*beads.Issue{ids[0]: {}}, nil
		})
		for _, c := range cands {
			kids, err := children(c.Spec.ID)
			if tc.fallback && (err != nil || len(kids) != 0) {
				t.Errorf("children(%s) after %v = %v, %v; want no children and no error", c.Spec.ID, tc.fail, kids, err)
			}
			if !tc.fallback && !errors.Is(err, errSpecChildrenTimeout) {
				t.Errorf("children(%s) after %v = %v; want the timeout", c.Spec.ID, tc.fail, err)
			}
		}
		if tc.fallback && (len(singles) != len(cands) || reads.Calls() > 2*specChildrenChunkSize) {
			t.Errorf("the refused chunk read %v per bead and cost %d calls; want each of its %d ids once and at most %d calls", singles, reads.Calls(), len(cands), 2*specChildrenChunkSize)
		}
		if !tc.fallback && (len(singles) != 0 || reads.Calls() != 1) {
			t.Errorf("the timed-out chunk (%v) read %v per bead and cost %d calls; want none and one", tc.fail, singles, reads.Calls())
		}
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

// chunkReadSpy counts the reads of each id a children read is given.
type chunkReadSpy struct{ counts map[string]int }

func (s *chunkReadSpy) read(_ string, ids []string) (map[string][]*beads.Issue, error) {
	kids := make(map[string][]*beads.Issue, len(ids))
	for _, id := range ids {
		s.counts[id]++
		kids[id] = nil
	}
	return kids, nil
}

// A loop that stops after the third candidate reads the first chunk and no id of the second (gt-4n5f3c).
func TestTickEffChunkNeverAskedNeverRead(t *testing.T) {
	t.Parallel()
	cands := specCandidateBoardRead(t, specCandidateFixture()).Candidates[:20]
	counts := map[string]int{}
	children := specCandidateChildren(t.TempDir(), newSpecTickReads(), cands, nil, (&chunkReadSpy{counts: counts}).read)
	for _, c := range cands[:3] {
		if _, err := children(c.Spec.ID); err != nil {
			t.Fatalf("children %s: %v", c.Spec.ID, err)
		}
	}
	for _, c := range cands[:specChildrenChunkSize] {
		if counts[c.Spec.ID] != 1 {
			t.Errorf("%s read %d times; want once, in the first chunk", c.Spec.ID, counts[c.Spec.ID])
		}
	}
	if len(counts) != specChildrenChunkSize {
		t.Errorf("read ids %v; want only the first chunk", counts)
	}
}

// An id the loop asks for twice is read once; dropping the served-from-memory check in specCandidateChildren turns this red (gt-4n5f3c).
func TestTickEffChunkReadOncePerTick(t *testing.T) {
	t.Parallel()
	cands := specCandidateBoardRead(t, specCandidateFixture()).Candidates[:20]
	counts := map[string]int{}
	children := specCandidateChildren(t.TempDir(), newSpecTickReads(), cands, nil, (&chunkReadSpy{counts: counts}).read)
	for i := 0; i < 2; i++ {
		if _, err := children(cands[3].Spec.ID); err != nil {
			t.Fatalf("ask %d for %s: %v", i+1, cands[3].Spec.ID, err)
		}
	}
	if got := counts[cands[3].Spec.ID]; got != 1 {
		t.Errorf("%s read %d times; want once, the second ask served from memory", cands[3].Spec.ID, got)
	}
}

// Nine wanted candidates asked for in order read eight, then the ninth alone:
// the chunk's read does not cover the boundary. A chunk size of nine reads all
// nine once and turns this red (gt-7fv8vp).
func TestTickEffChunkBoundary(t *testing.T) {
	t.Parallel()
	cands := specCandidateBoardRead(t, specCandidateFixture()).Candidates[:9]
	counts := map[string]int{}
	spy := &chunkReadSpy{counts: counts}
	var sizes []int
	read := func(dir string, ids []string) (map[string][]*beads.Issue, error) {
		sizes = append(sizes, len(ids))
		return spy.read(dir, ids)
	}
	children := specCandidateChildren(t.TempDir(), newSpecTickReads(), cands, nil, read)
	for _, c := range cands {
		if _, err := children(c.Spec.ID); err != nil {
			t.Fatalf("children %s: %v", c.Spec.ID, err)
		}
	}
	if want := []int{8, 1}; !slices.Equal(sizes, want) {
		t.Errorf("read sizes = %v; want %v: the ninth id costs its own read", sizes, want)
	}
	for _, c := range cands {
		if counts[c.Spec.ID] != 1 {
			t.Errorf("%s read %d times; want once across the two chunks", c.Spec.ID, counts[c.Spec.ID])
		}
	}
}
