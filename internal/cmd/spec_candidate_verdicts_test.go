package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specCandidateFixtureSize is the candidate count the call counts below are
// measured over: the size of the live candidate phase the bead was filed from
// (86 candidates at 12:37, gt-km9jt).
const specCandidateFixtureSize = 80

// specCandidateReadsBeforeGTKM9JT is the bd reads one tick's candidate scan
// made over the fixture before this change: the one details batch per rig plus
// one children read per candidate, eighty-one in all. It was measured at
// b4f678ef (the main this branch was cut from) through the counting store:
// details batches 1, children reads 80. TestSpecCandidateScanBeforeTheMemory
// measures the same number again with the verdict memory off and the pre-change
// per-bead children wiring, so the second tick is compared against a count this
// harness still measures rather than the fixture size restated.
const specCandidateReadsBeforeGTKM9JT = 81

// specCandidateFixture is eighty ready beads in one rig, as the board lists
// them.
func specCandidateFixture() []*beads.Issue {
	issues := make([]*beads.Issue, 0, specCandidateFixtureSize)
	for i := 0; i < specCandidateFixtureSize; i++ {
		issues = append(issues, &beads.Issue{
			ID:        fmt.Sprintf("gt-cand-%02d", i),
			Title:     fmt.Sprintf("Bead %d", i),
			Type:      "task",
			Status:    "open",
			Priority:  2,
			CreatedAt: "2026-09-29T10:00:00Z",
			UpdatedAt: fmt.Sprintf("2026-09-30T00:%02d:00Z", i),
		})
	}
	return issues
}

// specCandidateBoardRead runs the production intake over issues.
func specCandidateBoardRead(t *testing.T, issues []*beads.Issue) specBoardRead {
	t.Helper()
	read := specCandidates(specTown(t), 2, func(string) ([]*beads.Issue, error) { return issues, nil })
	if len(read.Errors) != 0 {
		t.Fatalf("board errors = %v", read.Errors)
	}
	return read
}

// verdictTick is one town's candidate scan: a fake board whose rows carry an
// updated_at and dependency count the memory can key on, a counting store for
// the details read, and a counting grouped reader for the children.
type verdictTick struct {
	root string
	rows map[string]string
	deps map[string]int
	// extra are candidates the board read carries beyond the fake's own, for a
	// bead the fake's candidate filter would hold out of it (a refusal).
	extra  []specCandidate
	f      *fakeSpecTown
	db     *beadsfake.Fake
	store  *countingStore
	reads  *specTickReads
	groups int
	kidIDs [][]string
	kids   map[string][]*beads.Issue
	kidErr error
	env    specDispatchEnv
}

// newVerdictTick is a tick over n clean beads. No seat is free, so a candidate
// that reaches the per-tick limit is skipped there: the tick runs every check
// up to the sling and hooks nothing between ticks.
func newVerdictTick(t *testing.T, n int) *verdictTick {
	t.Helper()
	v := &verdictTick{
		root:  t.TempDir(),
		rows:  map[string]string{},
		deps:  map[string]int{},
		reads: newSpecTickReads(),
		kids:  map[string][]*beads.Issue{},
	}
	db := beadsfake.New()
	f := newFakeSpecTown()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("gt-cand-%02d", i)
		f.specs[id] = cleanSpec(id, 2, "2026-09-29T10:00:00Z")
		v.rows[id] = fmt.Sprintf("2026-09-30T00:%02d:00Z", i)
		db.Seed(beads.Issue{ID: id, Design: "design of " + id})
	}
	v.f = f
	v.db = db
	v.store = newCountingStore(db)
	memory := newSpecAnnotatedTick(v.root, "daemon-1", func(string) beads.Client { return nil }).memory
	v.env = v.wire(f, newSpecCandidateVerdicts(memory,
		specCandidateInputsFor(f.env().Template, f.env().ShapeGate, f.env().MaxPriority,
			func(string) *specdispatch.TargetTree { return nil })))
	return v
}

// wire is the tick's candidate wiring over f: the board rows the memory keys
// on, the details read through the counting store, and the grouped children
// read through the counting reader.
func (v *verdictTick) wire(f *fakeSpecTown, verdicts *specCandidateVerdicts) specDispatchEnv {
	f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 0}}
	env := f.env()
	// The tick's own read ledger, as a fresh tick gets: the next tick must not
	// be served from the last tick's reads.
	reads := newSpecTickReads()
	v.reads = reads
	env.Reads = reads
	// No seat, so the per-tick limit (the free seats) is zero: every candidate
	// runs the checks below the lint and none is hooked between ticks.
	env.PerTick = 0
	env.Verdicts = verdicts
	board := env.Candidates
	var children func(beadID string) ([]specdispatch.Child, error)
	env.Candidates = func() specBoardRead {
		read := board()
		read.Candidates = append(read.Candidates, v.extra...)
		for i := range read.Candidates {
			id := read.Candidates[i].Spec.ID
			read.Candidates[i].UpdatedAt = v.rows[id]
			read.Candidates[i].DepCount = v.deps[id]
		}
		// The details read fills the design and the admission edges from the
		// store, exactly as the tick's own wiring does.
		read = specBoardDetails(reads, func(string) beads.Client { return v.store }, read, verdicts)
		if verdicts != nil && verdicts.memory != nil {
			verdicts.memory.observe(read)
		}
		children = specCandidateChildren(v.root, reads, read.Candidates, v.childRead)
		return read
	}
	env.Children = func(id string) ([]specdispatch.Child, error) {
		if children == nil {
			return nil, fmt.Errorf("children read before the board")
		}
		return children(id)
	}
	return env
}

// setBlocker gives id a blocking edge on blocker, which stands at status, and
// moves the board row's dependency count: the edge and its status are the
// details read's to answer, the count the board's.
func (v *verdictTick) setBlocker(t *testing.T, id, blocker, status string) {
	t.Helper()
	v.db.Seed(beads.Issue{ID: blocker, Status: status})
	if err := v.db.AddDependency(id, blocker); err != nil {
		t.Fatalf("adding the blocking edge: %v", err)
	}
	v.deps[id] = 1
}

// setBlockerStatus moves a blocker's status, leaving the dependent's row alone.
func (v *verdictTick) setBlockerStatus(blocker, status string) {
	v.db.Seed(beads.Issue{ID: blocker, Status: status})
}

// retown runs the same memory over a changed town: the next tick the daemon
// would make after an operator's edit.
func (v *verdictTick) retown(f *fakeSpecTown) specDispatchEnv {
	return v.retownWith(f, v.env.Verdicts)
}

// retownWith is retown over a given verdict memory, for a tick whose rules (and
// so whose inputs) differ.
func (v *verdictTick) retownWith(f *fakeSpecTown, verdicts *specCandidateVerdicts) specDispatchEnv {
	return v.wire(f, verdicts)
}

// childRead is the grouped children reader: it counts the grouped calls and
// answers from kids, or fails with kidErr.
func (v *verdictTick) childRead(_ string, ids []string) (map[string][]*beads.Issue, error) {
	v.groups++
	v.kidIDs = append(v.kidIDs, append([]string(nil), ids...))
	if v.kidErr != nil {
		return nil, v.kidErr
	}
	// bd keys every parent it was asked about, an empty list for a childless one.
	out := map[string][]*beads.Issue{}
	for _, id := range ids {
		out[id] = v.kids[id]
		if out[id] == nil {
			out[id] = []*beads.Issue{}
		}
	}
	return out, nil
}

// A tick over eighty candidates costs one details batch and one grouped
// children view, and a second tick over unchanged rows costs the same: the
// details are read every tick for the admission edges, and the shape verdicts
// the memory serves save the decision, not the read (gt-km9jt).
func TestSpecCandidateVerdictsSecondTickReadsOneBatchPerRig(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, specCandidateFixtureSize)

	first := runSpecDispatchCycle(v.env)
	if len(first.Skipped) != specCandidateFixtureSize {
		t.Fatalf("first tick skipped %d candidates, want all %d held for lack of a seat", len(first.Skipped), specCandidateFixtureSize)
	}
	if len(v.store.multis) != 1 || len(v.store.multis[0]) != specCandidateFixtureSize {
		t.Fatalf("first tick's details batches = %v, want the one batch over the fixture", v.store.multis)
	}
	if v.groups != 1 || len(v.kidIDs[0]) != specCandidateFixtureSize {
		t.Fatalf("first tick's children reads = %d calls %v, want one over the fixture", v.groups, v.kidIDs)
	}
	if got := v.reads.Calls(); got != 2 {
		t.Errorf("first tick counted %d bd reads, want the details view and the grouped children view", got)
	}

	runSpecDispatchCycle(v.retown(newFakeVerdictTown(v)))

	if len(v.store.multis) != 2 || len(v.store.multis[1]) != specCandidateFixtureSize {
		t.Fatalf("the second tick's details batches = %v, want one more batch over the fixture for its edges", v.store.multis)
	}
	if v.groups != 2 {
		t.Errorf("the second tick made %d children reads, want its one grouped view", v.groups-1)
	}
	if got := v.reads.Calls(); got != 2 {
		t.Errorf("the second tick counted %d bd reads, want its details batch and grouped children view", got)
	}
	// The phase costs at most half the bd reads a tick made before this change,
	// over the same fixture (gt-km9jt).
	if got := v.reads.Calls(); got > specCandidateReadsBeforeGTKM9JT/2 {
		t.Errorf("the second tick cost %d bd reads, want at most half the pre-change %d", got, specCandidateReadsBeforeGTKM9JT)
	}
}

// newFakeVerdictTown is a fresh fake over the same bead ids as a tick, for the
// next tick over a town whose decisions are unchanged.
func newFakeVerdictTown(v *verdictTick) *fakeSpecTown {
	f := newFakeSpecTown()
	for id := range v.rows {
		f.specs[id] = cleanSpec(id, 2, "2026-09-29T10:00:00Z")
	}
	return f
}

// The scan with no verdict memory and the pre-change per-bead children wiring
// costs the old number of bd reads, counted at the store: one details batch and
// one children read per candidate (gt-km9jt).
func TestSpecCandidateScanBeforeTheMemoryReadsChildrenPerBead(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, specCandidateFixtureSize)
	// No memory: every candidate is decided, and the children are read one bead
	// at a time the way the scan read them before this change.
	v.env.Verdicts = nil
	v.env.Children = func(id string) ([]specdispatch.Child, error) {
		kids, err := v.store.Children(id)
		if err != nil {
			return nil, err
		}
		return childSpecs(kids), nil
	}
	runSpecDispatchCycle(v.env)
	if got, want := len(v.store.multis)+v.store.children, specCandidateReadsBeforeGTKM9JT; got != want {
		t.Errorf("the pre-change wiring cost %d bd reads over the fixture, want the %d measured at b4f678ef", got, want)
	}
}

// A candidate whose row moved is read again, and the verdict the tick reaches
// is the new one: an edit that unshapes the bead is refused at the next tick
// (gt-km9jt).
func TestSpecCandidateVerdictsAreRereadWhenTheBeadIsEdited(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 1)
	id := "gt-cand-00"
	if r := runSpecDispatchCycle(v.env); len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specdispatch.SkipNoSeat) {
		t.Fatalf("first tick skipped %+v, want the seat hold", r.Skipped)
	}

	f := newFakeVerdictTown(v)
	shapeless := cleanSpec(id, 2, "2026-09-29T10:00:00Z")
	shapeless.Description = "## Goal\nonly a goal\n"
	f.specs[id] = shapeless
	v.rows[id] = "2026-09-30T01:00:00Z"

	r := runSpecDispatchCycle(v.retown(f))
	if len(v.store.multis) != 2 {
		t.Fatalf("the tick after the edit made %d details reads, want the moved bead read again", len(v.store.multis))
	}
	if len(r.Refused) != 1 || !strings.Contains(r.Refused[0].Line, "section missing") {
		t.Errorf("the tick after the edit refused %+v, want the missing section named", r.Refused)
	}
}

// A blocking edge added between two ticks is seen at the next one, though the
// bead's own fields did not move and its stored label and shape verdict still
// applies: the admission edges are the details read's, fresh every tick
// (gt-km9jt).
func TestSpecCandidateVerdictsSeeAnAddedEdge(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 1)
	id := "gt-cand-00"
	runSpecDispatchCycle(v.env)

	v.setBlocker(t, id, "gt-blocker", string(beads.StatusOpen))

	r := runSpecDispatchCycle(v.retown(v.f))
	if len(v.store.multis) != 2 {
		t.Fatalf("the tick after the edge made %d details reads, want the bead read again", len(v.store.multis))
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "blocked") {
		t.Errorf("the tick after the edge skipped %+v, want the admission refusal", r.Skipped)
	}
}

// A tick whose knobs differ reads every candidate again: the verdict is a
// function of the bead and the rules it was decided under, not of the bead
// alone (gt-km9jt).
func TestSpecCandidateVerdictsAreRereadWhenTheRulesChange(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 1)
	runSpecDispatchCycle(v.env)

	tighter := v.retownWith(newFakeVerdictTown(v), newSpecCandidateVerdicts(v.env.Verdicts.memory,
		specCandidateInputsFor(specdispatch.Template{Sections: append(append([]string(nil), v.env.Template.Sections...), "Notes"), Source: "built-in"},
			v.env.ShapeGate, v.env.MaxPriority, func(string) *specdispatch.TargetTree { return nil })))
	tighter.Template = specdispatch.Template{Sections: append(append([]string(nil), v.env.Template.Sections...), "Notes"), Source: "built-in"}

	r := runSpecDispatchCycle(tighter)
	if len(v.store.multis) != 2 {
		t.Fatalf("the tick under a new template made %d details reads, want every candidate read again", len(v.store.multis))
	}
	if len(r.Refused) != 1 || !strings.Contains(r.Refused[0].Line, "Notes") {
		t.Errorf("the tick under a new template refused %+v, want the new section named", r.Refused)
	}
}

// A candidate blocked at one tick is eligible at the next once its blocker
// closes: the blocker's change does not move the dependent's row, so the edges
// must be the tick's own read, never a served verdict (gt-km9jt).
func TestSpecCandidateVerdictsSeeABlockerClose(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 1)
	id := "gt-cand-00"
	v.setBlocker(t, id, "gt-blocker", string(beads.StatusOpen))

	if r := runSpecDispatchCycle(v.retown(v.f)); len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "blocked") {
		t.Fatalf("first tick skipped %+v, want the admission refusal", r.Skipped)
	}

	// The blocker closes; the dependent's own row does not move.
	v.setBlockerStatus("gt-blocker", string(beads.StatusClosed))
	r := runSpecDispatchCycle(v.retown(v.f))
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specdispatch.SkipNoSeat) {
		t.Errorf("the tick after the blocker closed skipped %+v, want the bead freed as far as the seat hold", r.Skipped)
	}
}

// A candidate free at one tick is blocked at the next once a blocker reopens:
// serving the edge verdict from the memory would sling it over the reopened
// blocker (gt-km9jt).
func TestSpecCandidateVerdictsSeeABlockerReopen(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 1)
	id := "gt-cand-00"
	// The edge is on the bead from the start, closed, so the dependency count
	// never moves: only the blocker's status does.
	v.setBlocker(t, id, "gt-blocker", string(beads.StatusClosed))

	if r := runSpecDispatchCycle(v.retown(v.f)); len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specdispatch.SkipNoSeat) {
		t.Fatalf("first tick skipped %+v, want the bead free of blockers as far as the seat hold", r.Skipped)
	}

	// The blocker reopens; the dependent's own row does not move.
	v.setBlockerStatus("gt-blocker", string(beads.StatusOpen))
	r := runSpecDispatchCycle(v.retown(v.f))
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "blocked") {
		t.Errorf("the tick after the blocker reopened skipped %+v, want the admission refusal", r.Skipped)
	}
}

// A candidate the board refuses on its own fields costs no details read once
// its unchanged row's refusal is known, and is read again the moment the row
// moves (gt-km9jt).
func TestSpecCandidateRefusalSkipsTheDetailsRead(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 0)
	id := "gt-held"
	// The hold is in the design, which only the details read carries: the row
	// itself reads as an ordinary open bead.
	v.extra = []specCandidate{{Spec: cleanSpec(id, 2, "2026-09-29T10:00:00Z"), Rig: "gastown"}}
	v.rows[id] = "2026-09-30T00:00:00Z"
	v.db.Seed(beads.Issue{ID: id, Design: "do not redispatch"})

	first := runSpecDispatchCycle(v.env)
	if len(first.Skipped) != 1 || !strings.Contains(first.Skipped[0].Line, specdispatch.SkipNotEligible) {
		t.Fatalf("first tick skipped %+v, want the hold's refusal", first.Skipped)
	}
	if len(v.store.multis) != 1 {
		t.Fatalf("first tick made %d details reads, want the one that found the hold", len(v.store.multis))
	}

	runSpecDispatchCycle(v.retown(v.f))
	if len(v.store.multis) != 1 {
		t.Errorf("the unchanged refused candidate cost a details read at the next tick")
	}

	// The row moves; the refusal is decided again from a fresh read.
	v.rows[id] = "2026-09-30T01:00:00Z"
	r := runSpecDispatchCycle(v.retown(v.f))
	if len(v.store.multis) != 2 {
		t.Errorf("the moved refused candidate was not read again")
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specdispatch.SkipNotEligible) {
		t.Errorf("the moved candidate skipped %+v, want the same refusal", r.Skipped)
	}
}

// A child's change is seen by the very next tick: the container hold reads the
// child set fresh each tick, never from a memory keyed on the parent's row
// (gt-km9jt).
func TestSpecCandidateChildrenSeeAChildChangeAtTheNextTick(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 1)
	id := "gt-cand-00"
	v.kids[id] = []*beads.Issue{{ID: id + ".1", Status: "open"}}

	if r := runSpecDispatchCycle(v.env); len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specdispatch.SkipContainer) {
		t.Fatalf("first tick skipped %+v, want the container hold", r.Skipped)
	}

	v.kids[id] = []*beads.Issue{{ID: id + ".1", Status: "closed"}}
	r := runSpecDispatchCycle(v.retown(newFakeVerdictTown(v)))
	if len(r.Skipped) != 1 || strings.Contains(r.Skipped[0].Line, specdispatch.SkipContainer) {
		t.Errorf("the tick after the child closed skipped %+v, want the container freed", r.Skipped)
	}
}

// The grouped children read answers eighty candidates with one view, and a
// grouped read that fails falls back to one read per bead so only the bead that
// cannot be read reports (gt-km9jt).
func TestSpecCandidateChildrenReadGrouped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	read := specCandidateBoardRead(t, specCandidateFixture())
	reads := newSpecTickReads()

	var groups int
	grouped := specCandidateChildren(root, reads, read.Candidates, func(_ string, ids []string) (map[string][]*beads.Issue, error) {
		groups++
		// bd keys every parent it was asked about, an empty list for a childless one.
		out := make(map[string][]*beads.Issue, len(ids))
		for _, id := range ids {
			out[id] = []*beads.Issue{}
		}
		return out, nil
	})
	for _, c := range read.Candidates {
		if kids, err := grouped(c.Spec.ID); err != nil || len(kids) != 0 {
			t.Fatalf("children %s = %v, %v; want the empty set", c.Spec.ID, kids, err)
		}
	}
	if groups != 1 {
		t.Errorf("eighty candidates cost %d grouped reads, want one", groups)
	}

	// A grouped read that fails falls back per bead and reports only the bead it
	// cannot answer.
	failing := specCandidateChildren(root, reads, read.Candidates, func(_ string, ids []string) (map[string][]*beads.Issue, error) {
		if len(ids) > 1 {
			return nil, fmt.Errorf("bd show: boom")
		}
		if ids[0] == "gt-cand-07" {
			return nil, fmt.Errorf("bd show: gone")
		}
		return map[string][]*beads.Issue{ids[0]: {}}, nil
	})
	if _, err := failing("gt-cand-00"); err != nil {
		t.Fatalf("a bead the fallback answered errored: %v", err)
	}
	if _, err := failing("gt-cand-07"); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("the bead the fallback could not answer = %v, want its own error", err)
	}
}

// The memory outlives a tick through the annotations file: a verdict saved by
// one tick is served to the next, and a bead the board no longer lists is
// dropped (gt-km9jt).
func TestSpecCandidateVerdictsPersistAndPrune(t *testing.T) {
	t.Parallel()
	v := newVerdictTick(t, 2)
	runSpecDispatchCycle(v.env)
	v.env.Verdicts.memory.save()

	reloaded := newSpecAnnotatedTick(v.root, "daemon-1", func(string) beads.Client { return nil }).memory
	if len(reloaded.verdicts) != 2 {
		t.Fatalf("the saved memory holds %d verdicts, want 2", len(reloaded.verdicts))
	}

	f := newFakeSpecTown()
	f.specs["gt-cand-00"] = cleanSpec("gt-cand-00", 2, "2026-09-29T10:00:00Z")
	next := v.wire(f, newSpecCandidateVerdicts(reloaded, v.env.Verdicts.inputs))
	runSpecDispatchCycle(next)
	reloaded.save()

	if _, ok := reloaded.verdicts["gt-cand-01"]; ok {
		t.Errorf("the memory still holds a bead the board no longer lists")
	}
	if _, ok := reloaded.verdicts["gt-cand-00"]; !ok {
		t.Errorf("the memory dropped a bead the board still lists")
	}
}

// An eligible record without a shape is never served — the tick that wrote it
// never got past the admission check — while a shaped one for an unchanged row
// is, and a row that moved is never served (gt-km9jt).
func TestSpecCandidateVerdictsServeOnlyAShapedRecordOfTheSameRow(t *testing.T) {
	t.Parallel()
	memory := newSpecAnnotatedTick(t.TempDir(), "daemon-1", func(string) beads.Client { return nil }).memory
	verdicts := newSpecCandidateVerdicts(memory, func(string) specCandidateInputs { return specCandidateInputs{} })
	c := specCandidate{Spec: specdispatch.Spec{ID: "gt-x"}, Rig: "gastown", UpdatedAt: "2026-09-30T00:00:00Z"}

	verdicts.record(c, specCandidateVerdict{Eligible: true})
	if _, served := verdicts.lookUp(c); served {
		t.Errorf("a record with no shape was served to the next tick")
	}
	verdicts.record(c, specCandidateVerdict{Eligible: true, Shaped: true})
	if _, served := verdicts.lookUp(c); !served {
		t.Errorf("an eligible, shaped verdict for an unchanged row was not served")
	}
	if _, served := verdicts.lookUp(specCandidate{Spec: specdispatch.Spec{ID: "gt-x"}, Rig: "gastown", UpdatedAt: "2026-09-30T00:01:00Z"}); served {
		t.Errorf("a verdict was served to a row that moved")
	}
	if _, served := verdicts.lookUp(specCandidate{Spec: specdispatch.Spec{ID: "gt-x"}, Rig: "gastown", UpdatedAt: "2026-09-30T00:00:00Z", DepCount: 1}); served {
		t.Errorf("a verdict was served to a row whose dependency count moved")
	}
}

// A candidate the tick found ineligible is remembered as such: its details are
// not read again, and the next tick skips it with the same reason (gt-km9jt).
func TestSpecCandidateVerdictsRememberAnIneligibleRow(t *testing.T) {
	t.Parallel()
	memory := newSpecAnnotatedTick(t.TempDir(), "daemon-1", func(string) beads.Client { return nil }).memory
	verdicts := newSpecCandidateVerdicts(memory, func(string) specCandidateInputs { return specCandidateInputs{} })
	c := specCandidate{Spec: specdispatch.Spec{ID: "gt-x"}, Rig: "gastown", UpdatedAt: "2026-09-30T00:00:00Z"}
	verdicts.record(c, specCandidateVerdict{Skip: "label needs-human"})

	entry, served := verdicts.lookUp(c)
	if !served || entry.Eligible || entry.Skip != "label needs-human" {
		t.Errorf("the ineligible verdict = %+v served=%v, want the stored refusal", entry, served)
	}
}

// The memory's file is the annotations file the tick already saves, so a tick
// with no owner keeps no verdicts either (gt-km9jt).
func TestSpecCandidateVerdictsWithoutAnOwnerAreNotRemembered(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := dispatch.SpecAnnotatedPath(root)
	memory := loadSpecAnnotated(path, "")
	verdicts := newSpecCandidateVerdicts(memory, func(string) specCandidateInputs { return specCandidateInputs{} })
	c := specCandidate{Spec: specdispatch.Spec{ID: "gt-x"}, UpdatedAt: "2026-09-30T00:00:00Z"}
	verdicts.record(c, specCandidateVerdict{Eligible: true, Shaped: true})
	memory.save()

	if reloaded := loadSpecAnnotated(path, "daemon-1"); len(reloaded.verdicts) != 0 {
		t.Errorf("a tick with no owner left %d verdicts behind", len(reloaded.verdicts))
	}
}

// A parent missing from a SUCCESSFUL grouped read was not answered (bd keys every
// parent, an empty list for a childless one), so it is read alone and never read
// as "no children": a held container's children must not vanish from the
// container hold. A read that still has nothing for it is an error. Mutation:
// treating the absence as an empty child set turns the first two rows red.
func TestSpecCandidateChildrenAbsentParentIsReadAloneNotEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	read := specCandidateBoardRead(t, specCandidateFixture())
	const parent = "gt-cand-05"
	child := &beads.Issue{ID: "gt-child-1", Status: "open"}

	var singles []string
	children := specCandidateChildren(root, newSpecTickReads(), read.Candidates, func(_ string, ids []string) (map[string][]*beads.Issue, error) {
		out := make(map[string][]*beads.Issue, len(ids))
		for _, id := range ids {
			if id == parent && len(ids) > 1 {
				continue // the grouped read leaves this parent out
			}
			out[id] = []*beads.Issue{}
		}
		if len(ids) == 1 && ids[0] == parent {
			singles = append(singles, ids[0])
			out[parent] = []*beads.Issue{child}
		}
		return out, nil
	})
	kids, err := children(parent)
	if err != nil || len(kids) != 1 || kids[0].ID != "gt-child-1" {
		t.Fatalf("children(%s) = %v, %v; want the child the single read found, not an empty set", parent, kids, err)
	}
	if len(singles) != 1 {
		t.Errorf("the absent parent was read alone %d times, want once", len(singles))
	}
	if kids, err := children("gt-cand-00"); err != nil || len(kids) != 0 {
		t.Errorf("an answered childless parent = %v, %v; want the empty set", kids, err)
	}

	// The read has nothing for the parent even alone: an error, not no children.
	silent := specCandidateChildren(root, newSpecTickReads(), read.Candidates, func(_ string, ids []string) (map[string][]*beads.Issue, error) {
		out := make(map[string][]*beads.Issue, len(ids))
		for _, id := range ids {
			if id != parent {
				out[id] = []*beads.Issue{}
			}
		}
		return out, nil
	})
	if _, err := silent(parent); err == nil || !strings.Contains(err.Error(), "answered nothing") {
		t.Errorf("a parent no read answers = %v, want an error naming it", err)
	}
}
