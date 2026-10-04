package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

type fakeQueueStore struct {
	ready, blocked, landing []*beads.Issue
	err                     error
	shown                   *beads.Issue
}

func (f *fakeQueueStore) ReadyAll() ([]*beads.Issue, error) { return f.ready, f.err }
func (f *fakeQueueStore) Blocked() ([]*beads.Issue, error)  { return f.blocked, f.err }
func (f *fakeQueueStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	return f.landing, f.err
}
func (f *fakeQueueStore) Show(id string) (*beads.Issue, error) {
	if f.shown == nil {
		return nil, errors.New("missing")
	}
	return f.shown, nil
}

func queueReaderOver(stores map[string]dashQueueStore, names ...string) *dashQueueReader {
	return &dashQueueReader{template: specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "test"}, stores: func() (map[string]dashQueueStore, []string, error) { return stores, names, nil }}
}

func TestQueueReadMergesStoresAndOrdersByPriority(t *testing.T) {
	t.Parallel()
	hq := &fakeQueueStore{}
	gastown := &fakeQueueStore{
		ready: []*beads.Issue{
			{ID: "gt-low", Title: "low", Priority: 3, CreatedAt: "2026-10-01T00:00:00Z"},
			{ID: "gt-urgent", Title: "urgent", Priority: 1, CreatedAt: "2026-10-02T00:00:00Z"},
			{ID: "gt-landing", Title: "already submitted", Priority: 1, CreatedAt: "2026-09-01T00:00:00Z"},
		},
		blocked: []*beads.Issue{{ID: "gt-blocked", Title: "waits", Priority: 2, BlockedBy: []string{"gt-a", "gt-b", "gt-c", "gt-d", "gt-e", "gt-f", "gt-g"}}},
		landing: []*beads.Issue{{ID: "gt-landing", Title: "already submitted", Status: "open", Labels: []string{land.LabelReadyToLand}}},
	}
	q := queueReaderOver(map[string]dashQueueStore{"hq": hq, "gastown": gastown}, "hq", "gastown").read(time.Now())
	if q == nil {
		t.Fatal("no queue")
	}
	if q.ReadyTotal != 2 || q.Ready[0].ID != "gt-urgent" || q.Ready[1].ID != "gt-low" {
		t.Errorf("ready = %+v (a bead already submitted to land is not also waiting to be built)", q.Ready)
	}
	if q.Ready[0].Rig != "gastown" {
		t.Errorf("a row must name its store: %+v", q.Ready[0])
	}
	if q.LandingTotal != 1 || q.Landing[0].ID != "gt-landing" {
		t.Errorf("landing = %+v", q.Landing)
	}
	if q.BlockedTotal != 1 || len(q.Blocked[0].BlockedBy) != 5 {
		t.Errorf("blocked = %+v (blockers are capped at five)", q.Blocked)
	}
	if len(q.Unreadable) != 0 {
		t.Errorf("unreadable = %v", q.Unreadable)
	}
}

// A store that cannot be read is named, never shown as an empty queue.
func TestQueueReadNamesAnUnreadableStore(t *testing.T) {
	t.Parallel()
	good := &fakeQueueStore{ready: []*beads.Issue{{ID: "gt-1", Title: "one"}}}
	bad := &fakeQueueStore{err: errors.New("dolt down")}
	q := queueReaderOver(map[string]dashQueueStore{"gastown": good, "beads": bad}, "gastown", "beads").read(time.Now())
	if q == nil || q.ReadyTotal != 1 {
		t.Fatalf("the readable store must still show: %+v", q)
	}
	if len(q.Unreadable) != 1 || q.Unreadable[0] != "beads" {
		t.Errorf("unreadable = %v, want [beads]", q.Unreadable)
	}
}

func TestQueueDetailOnlyReadsKnownStores(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{shown: &beads.Issue{
		ID: "gt-1", Title: "t", Description: "d", AcceptanceCriteria: "a", Notes: "n",
		CreatedAt: "2026-10-01T00:00:00Z", Comments: []beads.Comment{{Author: "x", Text: " hi ", CreatedAt: "2026-10-02T00:00:00Z"}},
	}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown")
	d, err := r.detail("gastown", "gt-1")
	if err != nil || d.Description != "d" || d.Acceptance != "a" || len(d.Comments) != 1 || d.Comments[0].Text != "hi" {
		t.Fatalf("detail = %+v err=%v", d, err)
	}
	if _, err := r.detail("somewhere-else", "gt-1"); err == nil {
		t.Error("a store the town does not have must be refused")
	}
}

func TestDashShapeNamesTheDispatchersVerdict(t *testing.T) {
	t.Parallel()
	tmpl := specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "test"}
	shaped := "## Goal\n\ng\n\n## Constraints\n\nc\n\n## Out of scope\n\no\n\n## Gate\n\nmake gate\n\n## Size\n\none worker, one landing\n\n## Acceptance\n\n- [ ] done\n"
	for name, c := range map[string]struct {
		issue      *beads.Issue
		shape, why string
	}{
		"shaped":         {&beads.Issue{ID: "gt-1", Type: "task", Description: shaped}, "ok", ""},
		"unshaped":       {&beads.Issue{ID: "gt-2", Type: "bug", Description: "just a note"}, "fix", "## Goal"},
		"needs planning": {&beads.Issue{ID: "gt-3", Type: "task", Description: shaped, Labels: []string{"needs-planning"}}, "planning", "label needs-planning"},
		"epic":           {&beads.Issue{ID: "gt-4", Type: "epic", Description: shaped}, "other", "type epic"},
	} {
		shape, note := dashShape(tmpl, c.issue)
		if shape != c.shape || (c.why != "" && !strings.Contains(note, c.why)) {
			t.Errorf("%s: shape=%q note=%q, want %q containing %q", name, shape, note, c.shape, c.why)
		}
	}
}

// A parked rig is stood down on purpose: the dispatcher does not serve it, so a
// well-shaped bead there is not dispatchable and must not wear the green badge.
func TestQueueMarksBeadsInParkedRigsNotDispatchable(t *testing.T) {
	t.Parallel()
	shaped := "## Goal\n\ng\n\n## Constraints\n\nc\n\n## Out of scope\n\no\n\n## Gate\n\nmake gate\n\n## Size\n\none worker, one landing\n\n## Acceptance\n\n- [ ] done\n"
	gastown := &fakeQueueStore{ready: []*beads.Issue{{ID: "gt-1", Title: "live", Type: "task", Description: shaped}}}
	mango := &fakeQueueStore{ready: []*beads.Issue{
		{ID: "ma-1", Title: "parked and shaped", Type: "task", Description: shaped},
		{ID: "ma-2", Title: "parked and unshaped", Type: "task", Description: "note"},
	}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": gastown, "mango": mango}, "gastown", "mango")
	r.parked = func(rig string) bool { return rig == "mango" }
	q := r.read(time.Now())
	byID := map[string]string{}
	for _, b := range q.Ready {
		byID[b.ID] = b.Shape
		if (b.Rig == "mango") != b.RigParked {
			t.Errorf("%s: RigParked=%v in rig %s", b.ID, b.RigParked, b.Rig)
		}
	}
	if byID["gt-1"] != "ok" {
		t.Errorf("a shaped bead in a live rig is dispatchable: %v", byID)
	}
	if byID["ma-1"] != "parked" {
		t.Errorf("a shaped bead in a parked rig must read parked, not ok: %v", byID)
	}
	if byID["ma-2"] != "fix" {
		t.Errorf("an unshaped bead keeps its shape verdict: %v", byID)
	}
	if len(q.ParkedRigs) != 1 || q.ParkedRigs[0] != "mango" {
		t.Errorf("parked rigs = %v", q.ParkedRigs)
	}
}
