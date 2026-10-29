package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	held                    []*beads.Issue // the work beads a seat holds: what ListIssueStatuses returns
	err, heldErr            error          // heldErr fails only the held-beads read
	shown                   *beads.Issue
}

func (f *fakeQueueStore) ReadyAll() ([]*beads.Issue, error) { return f.ready, f.err }
func (f *fakeQueueStore) Blocked() ([]*beads.Issue, error)  { return f.blocked, f.err }
func (f *fakeQueueStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	return f.landing, f.err
}
func (f *fakeQueueStore) ListIssueStatuses(...beads.IssueStatus) ([]*beads.Issue, error) {
	if f.heldErr != nil {
		return nil, f.heldErr
	}
	return f.held, f.err
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

// The blocked list is split into the beads waiting on another bead and the
// leaked molecule step wisps sitting in the same list (gt-vcr3z): the town's
// "121 blocked" was mostly wisps no patrol will ever advance, and counting
// them as blocked work hid the handful the operator is actually blocked on.
// Each row carries the verdict, so the Work panel's list hides exactly the
// rows its blocked line counted apart.
func TestQueueSplitsLeakedStepWispsFromBlockedWork(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{blocked: []*beads.Issue{
		{ID: "gt-real", Title: "waits on another bead", Type: "task", Priority: 2, BlockedBy: []string{"gt-x"}},
		{ID: "gt-wisp-4bac0", Title: "Self-review changes", Type: "task", Priority: 2, BlockedBy: []string{"gt-wisp-eutau"}},
		{ID: "gt-step-1", Title: "a step poured under a molecule", Type: "task", Parent: "gt-wisp-eutau", BlockedBy: []string{"gt-wisp-eutau"}},
		{ID: "gt-wisp-e9i8w", Title: "mol-polecat-work", Type: "molecule", Priority: 2, BlockedBy: []string{"gt-x"}},
	}}
	q := queueReaderOver(map[string]dashQueueStore{"hq": st}, "hq").read(time.Now())
	if q.BlockedTotal != 4 || q.BlockedWork != 1 || q.BlockedWisps != 3 {
		t.Errorf("blocked = %d work + %d wisps of %d, want 1 work and 3 wisps of 4", q.BlockedWork, q.BlockedWisps, q.BlockedTotal)
	}
	for _, row := range q.Blocked {
		if want := row.ID != "gt-real"; row.Wisp != want {
			t.Errorf("%s: wisp = %v, want %v", row.ID, row.Wisp, want)
		}
	}
}

// A step counts as leaked only when its molecule's work bead is terminal or no
// seat holds it (gt-xmys9). The steps of a molecule whose work bead is hooked
// to a seat are a live checklist: neither listed nor counted as leaked, and
// not blocked work either. A work bead that is closed, or open with nobody on
// it, leaves its steps leaked. The line's count and the list's rows come from
// one verdict, so they agree.
func TestQueueLeavesALiveMoleculesStepsOutOfTheLeak(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{
		held: []*beads.Issue{
			{ID: "gt-run", Status: "hooked", Assignee: "gastown/polecats/dag", Description: "attached_molecule: gt-wisp-live\nattached_formula: mol-polecat-work"},
			{ID: "gt-work", Status: "in_progress", Assignee: "gastown/polecats/rictus", Description: "attached_molecule: gt-wisp-busy"},
			// recorded on a bead nobody holds: not a seat's checklist
			{ID: "gt-orphan", Status: "hooked", Description: "attached_molecule: gt-wisp-orphan"},
		},
		blocked: []*beads.Issue{
			{ID: "gt-real", Title: "waits on another bead", Type: "task", Priority: 2, BlockedBy: []string{"gt-x"}},
			{ID: "gt-wisp-live", Title: "mol-polecat-work", Type: "molecule", BlockedBy: []string{"gt-run"}},
			{ID: "gt-wisp-l1", Title: "Self-review changes", Type: "task", Parent: "gt-wisp-live", BlockedBy: []string{"gt-wisp-live"}},
			{ID: "gt-wisp-l2", Title: "Submit work", Type: "task", Parent: "gt-wisp-live", BlockedBy: []string{"gt-wisp-l1"}},
			{ID: "gt-wisp-b1", Title: "Build", Type: "task", Parent: "gt-wisp-busy", BlockedBy: []string{"gt-wisp-busy"}},
			// the work bead closed, so no seat's list names this molecule
			{ID: "gt-wisp-d1", Title: "Self-review changes", Type: "task", Parent: "gt-wisp-closed", BlockedBy: []string{"gt-wisp-closed"}},
			{ID: "gt-wisp-d2", Title: "Submit work", Type: "task", Parent: "gt-wisp-closed", BlockedBy: []string{"gt-wisp-d1"}},
			{ID: "gt-wisp-o1", Title: "Build", Type: "task", Parent: "gt-wisp-orphan", BlockedBy: []string{"gt-wisp-orphan"}},
			{ID: "gt-wisp-n1", Title: "Build", Type: "task", Parent: "gt-wisp-unknown"},
		},
	}
	q := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown").read(time.Now())

	if q.BlockedTotal != 5 || q.BlockedWork != 1 || q.BlockedWisps != 4 {
		t.Errorf("blocked = %d work + %d wisps of %d, want 1 work and 4 wisps of 5 (the 4 live steps are in neither)", q.BlockedWork, q.BlockedWisps, q.BlockedTotal)
	}
	var listedWisps []string
	for _, row := range q.Blocked {
		if strings.HasPrefix(row.ID, "gt-wisp-l") || row.ID == "gt-wisp-b1" || row.ID == "gt-wisp-live" {
			t.Errorf("%s belongs to a molecule a seat holds, so it must not be listed", row.ID)
		}
		if row.Wisp {
			listedWisps = append(listedWisps, row.ID)
		}
	}
	// the line's count and the rows the list marks are one verdict
	if len(listedWisps) != q.BlockedWisps {
		t.Errorf("the line counts %d leaked wisps but the list marks %v", q.BlockedWisps, listedWisps)
	}
	for _, want := range []string{"gt-wisp-d1", "gt-wisp-d2", "gt-wisp-o1", "gt-wisp-n1"} {
		if !strings.Contains(strings.Join(listedWisps, " "), want) {
			t.Errorf("%s is a step of a molecule no seat holds and must be leaked; leaked = %v", want, listedWisps)
		}
	}
}

// A molecule is live wherever its work bead sits: a step in one store whose
// work bead is held in another is not leaked.
func TestQueueCountsAMoleculeLiveWhicheverStoreHoldsItsWorkBead(t *testing.T) {
	t.Parallel()
	hq := &fakeQueueStore{held: []*beads.Issue{
		{ID: "hq-run", Status: "hooked", Assignee: "gastown/polecats/dag", Description: "attached_molecule: gt-wisp-far"},
	}}
	gastown := &fakeQueueStore{blocked: []*beads.Issue{
		{ID: "gt-wisp-f1", Title: "Build", Type: "task", Parent: "gt-wisp-far", BlockedBy: []string{"gt-wisp-far"}},
	}}
	q := queueReaderOver(map[string]dashQueueStore{"hq": hq, "gastown": gastown}, "hq", "gastown").read(time.Now())
	if q.BlockedTotal != 0 || q.BlockedWisps != 0 {
		t.Errorf("blocked = %d (%d wisps), want 0: the step belongs to a molecule hq's work bead holds", q.BlockedTotal, q.BlockedWisps)
	}
}

// A store whose held beads cannot be read is named unreadable, not read as if
// no seat held anything and every live step leaked.
func TestQueueNamesAStoreWhoseHeldBeadsFailToRead(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{heldErr: errors.New("dolt down"), blocked: []*beads.Issue{{ID: "gt-wisp-a", Type: "task", Parent: "gt-wisp-m"}}}
	q := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown").read(time.Now())
	if q.BlockedWisps != 0 || len(q.Unreadable) != 1 {
		t.Errorf("wisps = %d, unreadable = %v", q.BlockedWisps, q.Unreadable)
	}
}

// The split is counted over the whole blocked list, not the capped rows the
// page is handed: a store with more blocked beads than the list keeps still
// reports work and wisps that add up to its total (gt-vcr3z).
func TestQueueSplitsWispsPastTheRowCap(t *testing.T) {
	t.Parallel()
	var blocked []*beads.Issue
	for i := 0; i < 44; i++ {
		blocked = append(blocked, &beads.Issue{ID: fmt.Sprintf("gt-wisp-%02d", i), Title: "Self-review changes", Type: "task", Priority: 2})
	}
	blocked = append(blocked, &beads.Issue{ID: "gt-real", Title: "waits", Type: "task", Priority: 2, BlockedBy: []string{"gt-x"}})
	q := queueReaderOver(map[string]dashQueueStore{"hq": &fakeQueueStore{blocked: blocked}}, "hq").read(time.Now())
	if q.BlockedTotal != 45 || q.BlockedWork != 1 || q.BlockedWisps != 44 {
		t.Errorf("blocked = %d work + %d wisps of %d, want 1 and 44 of 45", q.BlockedWork, q.BlockedWisps, q.BlockedTotal)
	}
	if len(q.Blocked) >= q.BlockedTotal {
		t.Errorf("the rows were not capped (%d of %d), so the test proves nothing about counting past the cap", len(q.Blocked), q.BlockedTotal)
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

// The page links a bead id written in the overseer's report, and it can only do
// that from the queue's payload: the reader hands it each store's beads prefix
// beside the store's name (gt-7005n).
func TestQueueCarriesTheStoresPrefixes(t *testing.T) {
	t.Parallel()

	r := queueReaderOver(map[string]dashQueueStore{"hq": &fakeQueueStore{}, "gastown": &fakeQueueStore{}}, "hq", "gastown")
	r.prefixes = func(names []string) map[string]string {
		return map[string]string{"hq": "hq", "gt": "gastown"}
	}
	q := r.read(time.Now())
	if q == nil {
		t.Fatal("no queue")
	}
	if q.Prefixes["hq"] != "hq" || q.Prefixes["gt"] != "gastown" || len(q.Prefixes) != 2 {
		t.Errorf("prefixes = %v, want hq->hq and gt->gastown", q.Prefixes)
	}
	if b, err := json.Marshal(q); err != nil || !strings.Contains(string(b), `"prefixes":{"gt":"gastown"`) {
		t.Errorf("the prefixes do not reach the page: %s err=%v", b, err)
	}
	// A reader that resolves no prefixes is left as it was: the page draws the
	// report's ids as text rather than offering a link nothing can open.
	if p := queueReaderOver(map[string]dashQueueStore{"gastown": &fakeQueueStore{}}, "gastown").read(time.Now()); p == nil || len(p.Prefixes) != 0 {
		t.Errorf("a reader with no prefix source must report none: %+v", p)
	}
}

// storePrefixes reads the town's routes file, where every rig's prefix is
// recorded, and names the town store hq — the prefix its own ids carry — so a
// report's "hq-…" opens too. A prefix naming a store the reader does not carry
// is left out, since a link to it could only fail (gt-7005n).
func TestStorePrefixesReadsTheTownRoutes(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"hq-","path":"."}
{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"dv-","path":"devops/mayor/rig"}
{"prefix":"zz-","path":"zzz/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(dir, beads.RoutesFileName), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}

	got := storePrefixes(townRoot, []string{"hq", "gastown", "devops"})
	want := map[string]string{"hq": "hq", "gt": "gastown", "dv": "devops"}
	if len(got) != len(want) {
		t.Fatalf("prefixes = %v, want %v", got, want)
	}
	for prefix, rig := range want {
		if got[prefix] != rig {
			t.Errorf("prefix %q = %q, want %q", prefix, got[prefix], rig)
		}
	}
	if _, ok := got["zz"]; ok {
		t.Error("a prefix for a store the reader does not carry must not be offered as a link")
	}
}

// dashesShape is a description that passes the dispatcher's shape lint, so a
// test's bead reads "ok" from the lint alone and the verdict overlay is the
// only thing that can change it.
const dashesShape = "## Goal\n\ng\n\n## Constraints\n\nc\n\n## Out of scope\n\no\n\n## Gate\n\nmake gate\n\n## Size\n\none worker, one landing\n\n## Acceptance\n\n- [ ] done\n"

// The dispatcher's own last verdict outranks a shape read off the bead's text
// (gt-ckncx): a bead that lints clean and that the last tick skipped is not
// dispatchable, and the row carries the tick's reason, when the run began and
// for how many ticks.
func TestDashQueueOverlaysTheDispatchersSkippedVerdict(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	gastown := &fakeQueueStore{ready: []*beads.Issue{
		{ID: "gt-1", Title: "behind a bead in progress", Type: "task", Description: dashesShape},
	}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": gastown}, "gastown")
	r.verdicts = func() (*specdispatch.Ledger, error) {
		return &specdispatch.Ledger{SavedAt: since.Add(time.Minute), Verdicts: map[string]specdispatch.Verdict{
			"gt-1": {Bead: "gt-1", Rig: "gastown", Class: specVerdictSkipped,
				Reason: "overlap hold: gt-2 is in progress", FirstAt: since, Ticks: 3},
		}}, nil
	}
	q := r.read(since.Add(2 * time.Minute))
	if q == nil || len(q.Ready) != 1 {
		t.Fatalf("ready = %+v", q)
	}
	row := q.Ready[0]
	if row.Shape != "skipped" || row.ShapeNote != "overlap hold: gt-2 is in progress" {
		t.Errorf("shape = %q note = %q, want skipped and the tick's reason", row.Shape, row.ShapeNote)
	}
	if !row.SkippedSince.Equal(since) || row.SkipTicks != 3 {
		t.Errorf("skipped since %v over %d ticks, want %v over 3", row.SkippedSince, row.SkipTicks, since)
	}
	if row.SkipClass != specVerdictSkipped || row.SkipReason != "overlap hold: gt-2 is in progress" {
		t.Errorf("skip class/reason = %q/%q", row.SkipClass, row.SkipReason)
	}
	if !q.VerdictsAt.Equal(since.Add(time.Minute)) || q.VerdictsStale {
		t.Errorf("verdicts at %v stale=%v, want the tick's save time and fresh", q.VerdictsAt, q.VerdictsStale)
	}
}

// A shape the lint itself decided against is not overwritten by the ledger: an
// assigned bead stays held and a parked rig's bead stays parked, each still
// carrying the verdict's fields so the page can say why the dispatcher passed
// it over. A class that is not an exclusion fills the fields only.
func TestDashQueueKeepsAHeldOrParkedShapeWithTheVerdict(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	gastown := &fakeQueueStore{ready: []*beads.Issue{
		{ID: "gt-held", Title: "someone is on it", Type: "task", Description: dashesShape, Assignee: "gastown/crew/sloan"},
		{ID: "gt-rough", Title: "unshaped", Type: "task", Description: "just a note"},
	}}
	mango := &fakeQueueStore{ready: []*beads.Issue{
		{ID: "ma-1", Title: "parked", Type: "task", Description: dashesShape},
	}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": gastown, "mango": mango}, "gastown", "mango")
	r.parked = func(rig string) bool { return rig == "mango" }
	r.verdicts = func() (*specdispatch.Ledger, error) {
		return &specdispatch.Ledger{SavedAt: at, Verdicts: map[string]specdispatch.Verdict{
			"gt-held":  {Bead: "gt-held", Class: specVerdictSkipped, Reason: "overlap hold", FirstAt: at, Ticks: 1},
			"gt-rough": {Bead: "gt-rough", Class: specVerdictFailed, Reason: "the tick failed it", FirstAt: at, Ticks: 2},
			"ma-1":     {Bead: "ma-1", Class: specVerdictDeferred, Reason: "the seat is busy", FirstAt: at, Ticks: 4},
		}}, nil
	}
	q := r.read(at.Add(time.Minute))
	rows := map[string]dashboard.QueueBead{}
	for _, b := range q.Ready {
		rows[b.ID] = b
	}
	for id, want := range map[string]string{"gt-held": "held", "gt-rough": "fix", "ma-1": "parked"} {
		if got := rows[id].Shape; got != want {
			t.Errorf("%s: shape = %q, want %q kept", id, got, want)
		}
		if rows[id].SkipClass == "" || rows[id].SkipReason == "" {
			t.Errorf("%s: keeps its shape but must still carry the verdict: %+v", id, rows[id])
		}
	}
	if got := rows["ma-1"]; got.SkipClass != specVerdictDeferred || got.SkipTicks != 4 || !got.SkippedSince.Equal(at) {
		t.Errorf("ma-1 skip fields = %+v, want deferred over 4 ticks from %v", got, at)
	}
}

// A planning verdict fills the row's skip fields and changes no shape: the bead
// lints clean, so it stays "ok" with the dispatcher's reason beside it, and the
// page can say why the dispatcher will not take it yet (gt-ckncx).
func TestDashQueueFillsThePlanningVerdictWithoutChangingTheShape(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := &fakeQueueStore{ready: []*beads.Issue{{ID: "gt-1", Title: "needs planning", Type: "task", Description: dashesShape}}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown")
	r.verdicts = func() (*specdispatch.Ledger, error) {
		return &specdispatch.Ledger{SavedAt: at, Verdicts: map[string]specdispatch.Verdict{
			"gt-1": {Bead: "gt-1", Class: specVerdictPlanning, Reason: "label needs-planning", FirstAt: at, Ticks: 2},
		}}, nil
	}
	q := r.read(at.Add(time.Minute))
	if q == nil || len(q.Ready) != 1 {
		t.Fatalf("ready = %+v", q)
	}
	row := q.Ready[0]
	if row.Shape != "ok" || row.ShapeNote != "" {
		t.Errorf("shape = %q note = %q, want the lint's ok unchanged", row.Shape, row.ShapeNote)
	}
	if row.SkipClass != specVerdictPlanning || row.SkipReason != "label needs-planning" || row.SkipTicks != 2 || !row.SkippedSince.Equal(at) {
		t.Errorf("skip fields = %+v, want planning over 2 ticks from %v", row, at)
	}
}

// A dispatcher that is held or stopped stops writing, so its last tick's
// verdicts read as current would be a claim nothing backs: older than the
// window, the ledger changes no shape and fills no field, and the queue says
// the verdicts are old instead (gt-ckncx).
func TestDashQueueIgnoresAnOldVerdictLedger(t *testing.T) {
	t.Parallel()
	saved := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := &fakeQueueStore{ready: []*beads.Issue{{ID: "gt-1", Title: "shaped", Type: "task", Description: dashesShape}}}
	r := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown")
	r.verdicts = func() (*specdispatch.Ledger, error) {
		return &specdispatch.Ledger{SavedAt: saved, Verdicts: map[string]specdispatch.Verdict{
			"gt-1": {Bead: "gt-1", Class: specVerdictSkipped, Reason: "overlap hold", FirstAt: saved, Ticks: 2},
		}}, nil
	}
	q := r.read(saved.Add(dashVerdictTTL + time.Minute))
	if q == nil || len(q.Ready) != 1 {
		t.Fatalf("ready = %+v", q)
	}
	if row := q.Ready[0]; row.Shape != "ok" || row.SkipClass != "" || row.SkipReason != "" || row.SkipTicks != 0 || !row.SkippedSince.IsZero() {
		t.Errorf("an old verdict must not be presented as current: %+v", row)
	}
	if !q.VerdictsStale || !q.VerdictsAt.Equal(saved) {
		t.Errorf("verdicts stale=%v at=%v, want stale at the ledger's save time %v", q.VerdictsStale, q.VerdictsAt, saved)
	}
}

// A ledger with no tick in it sets neither verdict field, so the page can tell
// "no verdict yet" from a fresh one; a ledger this read cannot load overlays
// nothing and is marked stale, so the page never offers the lint's shape as the
// dispatcher's truth on the strength of a record it could not read (gt-ckncx).
func TestDashQueueIgnoresAVerdictLedgerItCannotRead(t *testing.T) {
	t.Parallel()
	st := &fakeQueueStore{ready: []*beads.Issue{{ID: "gt-1", Title: "shaped", Type: "task", Description: dashesShape}}}
	for name, c := range map[string]struct {
		load      func() (*specdispatch.Ledger, error)
		wantStale bool
	}{
		"missing": {func() (*specdispatch.Ledger, error) {
			return specdispatch.LoadLedger(filepath.Join(t.TempDir(), "verdicts.json"))
		}, false},
		"nil":        {func() (*specdispatch.Ledger, error) { return nil, nil }, false},
		"unreadable": {func() (*specdispatch.Ledger, error) { return nil, errors.New("dolt down") }, true},
	} {
		r := queueReaderOver(map[string]dashQueueStore{"gastown": st}, "gastown")
		r.verdicts = c.load
		q := r.read(time.Now())
		if q == nil || len(q.Ready) != 1 {
			t.Fatalf("%s: ready = %+v", name, q)
		}
		if row := q.Ready[0]; row.Shape != "ok" || row.SkipClass != "" {
			t.Errorf("%s: shape = %q skip class = %q, want the lint's ok and no verdict", name, row.Shape, row.SkipClass)
		}
		if !q.VerdictsAt.IsZero() || q.VerdictsStale != c.wantStale {
			t.Errorf("%s: verdicts at %v stale=%v, want no time and stale=%v", name, q.VerdictsAt, q.VerdictsStale, c.wantStale)
		}
	}
}
