package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
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

// A bead waiting to land is aged from its last update rather than from when it
// was filed: the label write gt done makes is normally the bead's last write, so
// the Landings pane reads that as the moment the wait started (gt-zc45r).
func TestQueueRowCarriesTheBeadsLastUpdate(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{landing: []*beads.Issue{{
		ID: "gt-1", Title: "submitted", Status: "open", Labels: []string{land.LabelReadyToLand},
		CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-02T03:04:05Z",
	}}}
	q := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown").read(time.Now())
	if q == nil || len(q.Landing) != 1 {
		t.Fatalf("landing = %+v", q)
	}
	row := q.Landing[0]
	if want := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC); !row.UpdatedAt.Equal(want) {
		t.Errorf("updated_at = %v, want the bead's last write %v", row.UpdatedAt, want)
	}
	if b, err := json.Marshal(row); err != nil || !strings.Contains(string(b), `"updated_at":"2026-10-02T03:04:05Z"`) {
		t.Errorf("a waiting row does not carry updated_at to the page: %s err=%v", b, err)
	}
	// A bead that records no update leaves the field out rather than sending a
	// zero time the page would read as 1970.
	if b, err := json.Marshal(dashboard.QueueBead{ID: "gt-2"}); err != nil || strings.Contains(string(b), "updated_at") {
		t.Errorf("a row with no update must omit updated_at: %s err=%v", b, err)
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

// The Rigs panel's rows come from the same read as the queue: one per known
// rig in registry order, hq left out (it is the town store, not a rig), and a
// store this read could not reach left blank rather than shown as empty.
func TestQueueReadFillsOneRigRowPerStore(t *testing.T) {
	t.Parallel()
	gastown := &fakeQueueStore{
		ready:   []*beads.Issue{{ID: "gt-1", Title: "one"}, {ID: "gt-2", Title: "two"}},
		landing: []*beads.Issue{{ID: "gt-3", Title: "submitted", Status: "open", Labels: []string{land.LabelReadyToLand}}},
	}
	mango := &fakeQueueStore{ready: []*beads.Issue{{ID: "ma-1", Title: "parked work"}}}
	doltDown := &fakeQueueStore{err: errors.New("dolt down")}
	r := queueReaderOver(map[string]dashQueueStore{
		"hq": &fakeQueueStore{}, "gastown": gastown, "mango": mango, "beads": doltDown,
	}, "hq", "gastown", "mango", "beads")
	r.parked = func(rig string) bool { return rig == "mango" }
	q := r.read(time.Now())

	var names []string
	for _, row := range q.Rigs {
		names = append(names, row.Name)
		switch row.Name {
		case "gastown":
			if row.Parked || row.Ready == nil || *row.Ready != 2 || row.Landing == nil || *row.Landing != 1 {
				t.Errorf("gastown row = %+v", row)
			}
		case "mango":
			if !row.Parked || row.Ready == nil || *row.Ready != 1 || row.Landing == nil || *row.Landing != 0 {
				t.Errorf("mango row = %+v (a parked rig keeps its row and its numbers)", row)
			}
		case "beads":
			if row.Ready != nil || row.Landing != nil {
				t.Errorf("an unreadable store must leave its counts unset, not zero: %+v", row)
			}
		}
	}
	if strings.Join(names, ",") != "gastown,mango,beads" {
		t.Errorf("rig rows = %v, want one per known rig and no hq", names)
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

// A bead assigned to someone is theirs, not free work: the dispatcher takes
// only unassigned beads, so an assigned bead reads held whether or not it is
// shaped, and it stays in the list because it is work in progress rather than
// work nobody can pick up. A parked rig still outranks the assignee.
func TestQueueHoldsBeadsAssignedToSomeone(t *testing.T) {
	t.Parallel()
	shaped := "## Goal\n\ng\n\n## Constraints\n\nc\n\n## Out of scope\n\no\n\n## Gate\n\nmake gate\n\n## Size\n\none worker, one landing\n\n## Acceptance\n\n- [ ] done\n"
	gastown := &fakeQueueStore{ready: []*beads.Issue{
		{ID: "gt-held-shaped", Title: "someone is on it", Type: "task", Description: shaped, Assignee: "gastown/crew/sloan"},
		{ID: "gt-held-rough", Title: "someone is shaping it", Type: "task", Description: "note", Assignee: "gastown/crew/sloan"},
		{ID: "gt-free-shaped", Title: "free", Type: "task", Description: shaped},
		{ID: "gt-free-rough", Title: "free and rough", Type: "task", Description: "note"},
	}}
	mango := &fakeQueueStore{ready: []*beads.Issue{
		{ID: "ma-1", Title: "parked and assigned", Type: "task", Description: shaped, Assignee: "mango/crew/x"},
	}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": gastown, "mango": mango}, "gastown", "mango")
	r.parked = func(rig string) bool { return rig == "mango" }
	q := r.read(time.Now())
	shape, note := map[string]string{}, map[string]string{}
	for _, b := range q.Ready {
		shape[b.ID], note[b.ID] = b.Shape, b.ShapeNote
	}
	for id, want := range map[string]string{
		"gt-held-shaped": "held",
		"gt-held-rough":  "held",
		"gt-free-shaped": "ok",
		"gt-free-rough":  "fix",
		"ma-1":           "parked",
	} {
		if shape[id] != want {
			t.Errorf("%s: shape = %q, want %q", id, shape[id], want)
		}
	}
	if n := note["gt-held-shaped"]; !strings.Contains(n, "gastown/crew/sloan") || !strings.Contains(n, "unassigned") {
		t.Errorf("a held row's note names the assignee and the rule: %q", n)
	}
}
