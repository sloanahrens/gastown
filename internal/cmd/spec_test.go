package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/townconfig"
)

const specTestDescription = "## Goal\nA thing.\n\n## Constraints\nGo.\n\n## Out of scope\nNone.\n\n## Gate\nmake gate\n\n## Size\none worker, one MR"

// specTestAcceptance is the acceptance list a shapely test bead carries: the
// lint's minimum is three items.
const specTestAcceptance = "- [ ] a\n- [ ] b\n- [ ] c"

// specConstraints is specTestDescription with its Constraints body replaced by
// body, or the section dropped when body is nil, so a fixture that changes that
// section does not depend on the wording it replaces (gt-zs8fq).
func specConstraints(body *string) string {
	var out []string
	in := false
	for _, line := range strings.Split(specTestDescription, "\n") {
		if name, ok := strings.CutPrefix(line, "## "); ok {
			in = strings.EqualFold(strings.TrimSpace(name), "Constraints")
			if in && body == nil {
				continue
			}
			out = append(out, line)
			if in {
				out = append(out, *body)
			}
			continue
		}
		if !in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// cleanSpec is a plain work bead: type task, no labels. Shape is the whole
// gate, so it must be dispatchable as it stands (gt-mmsr2).
func cleanSpec(id string, priority int, created string) specdispatch.Spec {
	return specdispatch.Spec{
		ID: id, Title: "Add " + id, Type: "task", Status: "open", Priority: priority, CreatedAt: created,
		Description: specTestDescription, Acceptance: specTestAcceptance,
	}
}

// fakeSpecTown records every side effect a dispatcher tick makes.
type fakeSpecTown struct {
	specs     map[string]specdispatch.Spec
	order     []string
	roster    specRoster
	rosterErr error
	hold      string
	rigHold   map[string]string
	// rigOf is each bead's rig; a bead absent here is in gastown, the rig most
	// fixtures use.
	rigOf  map[string]string
	revert map[string]*specdispatch.Revert
	// children is each bead's direct child set, and childErrs a per-bead
	// children read failure, for the container rule.
	children map[string][]specdispatch.Child
	// blockers is each bead's blocking edges as the board's detail read returns
	// them, for the admission check (gt-ndad8). The fake hands them to the tick
	// whole, so specBoardDetails, which fills them in production, is outside its
	// path: TestSpecDispatchAdmitsOnTheEdgesTheBoardDetailsReadFilled covers that
	// wiring (gt-acn51). An id absent here is a bead whose read answered no
	// dependencies.
	blockers map[string][]beads.IssueDep
	// showOverride is the full read the tick sees when it differs from the
	// board snapshot: the board lagged a note the read carries (gt-kr5xv).
	// An id absent here falls back to specs, so the common case needs no entry.
	showOverride map[string]specdispatch.Spec
	// showSeq is what a bead's successive reads return, consumed in order with
	// the last entry repeating: the first read is the tick's re-check of the
	// board snapshot, the one before the sling is the last-moment re-read
	// (gt-01gix). An id absent here reads from showOverride, then specs.
	showSeq   map[string][]specRead
	childErrs map[string]error
	slingErrs map[string][]error // per bead, consumed in order
	slung     []string
	// slingSeats is the seat each sling went to, positionally with slung.
	slingSeats []specdispatch.SeatChoice
	// slungResume is the resume branch each sling carried, positionally with
	// slung: "" when the dispatch started fresh.
	slungResume []string
	// slungRuns is the formula, vars and actor each sling carried, positionally
	// with slung (gt-j5cr2).
	slungRuns []specdispatch.FormulaRun
	// missingFormulas names the formulas FormulaExists refuses.
	missingFormulas map[string]bool
	// originBranches is what BranchOnOrigin finds, keyed "rig/branch"; an
	// absent key is a branch that is gone from origin.
	originBranches map[string]bool
	originErr      error
	notes          map[string][]string
	labels         map[string][]string
	sleeps         int
	slingRecords   []specSlingRecord
	// mu guards the fields the tick's concurrent slings touch (the show
	// sequence, the sling record, sleeps); the rest run on the tick goroutine.
	mu sync.Mutex
	// seats overrides the tick's budget seats; nil is the default pool, which
	// reserves no label and so holds a needs-pro bead (gt-lxxo4).
	seats []specdispatch.Seat
	// maxTotal is polecat_pool.max_total as the tick reads it: 0 is a town
	// with no total cap, which is what the budget tests without one get.
	maxTotal int
	// omThreshold is each rig's review threshold as the overrun box reads it,
	// omThresholdErr a rig whose read fails, and omThresholdReads how many
	// times the tick asked for each rig (gt-b9j3o.4). A rig in neither map
	// sets no threshold.
	omThreshold      map[string]float64
	omThresholdErr   map[string]error
	omThresholdReads map[string]int
	// targets is each rig's target inventory as the lint's path check reads it
	// (gt-7t1im). A rig absent here asks no path question, which is what a
	// fixture that does not care about one gets.
	targets map[string]*specdispatch.TargetTree
}

// specTestNow is the tick clock every dispatch test runs at.
var specTestNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// specRead is one full read of a bead: the spec Show returns, or the error it
// fails with, so a sequence can pin a bead that changes between the tick's
// re-check and the sling.
type specRead struct {
	spec specdispatch.Spec
	err  error
}

// specSlingRecord is one sling the fake saw, kept in candidate order.
type specSlingRecord struct {
	order  int
	id     string
	seat   specdispatch.SeatChoice
	resume string
	run    specdispatch.FormulaRun
}

// rigOfBead is id's rig, gastown unless the test says otherwise.
func (f *fakeSpecTown) rigOfBead(id string) string {
	if rig := f.rigOf[id]; rig != "" {
		return rig
	}
	return "gastown"
}

func newFakeSpecTown(specs ...specdispatch.Spec) *fakeSpecTown {
	f := &fakeSpecTown{specs: map[string]specdispatch.Spec{}, rigHold: map[string]string{}, revert: map[string]*specdispatch.Revert{},
		slingErrs: map[string][]error{}, originBranches: map[string]bool{}, notes: map[string][]string{}, labels: map[string][]string{},
		children: map[string][]specdispatch.Child{}, childErrs: map[string]error{}, showSeq: map[string][]specRead{}, blockers: map[string][]beads.IssueDep{},
		omThreshold: map[string]float64{}, omThresholdErr: map[string]error{}, omThresholdReads: map[string]int{}}
	for _, s := range specs {
		f.specs[s.ID] = s
	}
	return f
}

func (f *fakeSpecTown) env() specDispatchEnv {
	seats := f.seats
	if seats == nil {
		seats = []specdispatch.Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "claude-sonnet", Cap: 2},
		}
	}
	// A fixed seed keeps the seat draw the same on every run.
	budget := specdispatch.Budget{Seats: seats, MaxTotal: f.maxTotal, Rand: rand.New(rand.NewSource(1))}
	return specDispatchEnv{
		Hold: func() string { return f.hold },
		Candidates: func() specBoardRead {
			var list []specdispatch.Spec
			var skipped []specDispatchEntry
			labeled := 0
			for _, s := range f.specs {
				if s.HasLabel(specdispatch.DispatchFailedLabel) {
					labeled++
					continue
				}
				if ok, why := specdispatch.Eligible(s, 2); ok {
					list = append(list, s)
				} else if specdispatch.BoardCarried(s) {
					// specCandidates reports a board-carried refusal rather
					// than dropping it unseen (gt-k1g3e).
					skipped = append(skipped, specDispatchEntry{Bead: s.ID, Rig: f.rigOfBead(s.ID), Line: fmt.Sprintf("%s: not eligible (%s)", s.ID, why)})
				}
			}
			specdispatch.Order(list)
			sort.Slice(skipped, func(i, j int) bool { return skipped[i].Bead < skipped[j].Bead })
			out := make([]specCandidate, 0, len(list))
			for _, s := range list {
				out = append(out, specCandidate{Spec: s, Rig: f.rigOfBead(s.ID), Blockers: f.blockers[s.ID]})
			}
			return specBoardRead{Candidates: out, LabeledFailed: labeled, Skipped: skipped}
		},
		Show: func(id string) (specdispatch.Spec, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if seq := f.showSeq[id]; len(seq) > 0 {
				r := seq[0]
				if len(seq) > 1 {
					f.showSeq[id] = seq[1:]
				}
				return r.spec, r.err
			}
			if s, ok := f.showOverride[id]; ok {
				return s, nil
			}
			s, ok := f.specs[id]
			if !ok {
				return s, errors.New("not found")
			}
			return s, nil
		},
		Children: func(id string) ([]specdispatch.Child, error) {
			if err := f.childErrs[id]; err != nil {
				return nil, err
			}
			return f.children[id], nil
		},
		RigHold: func(rig string) string { return f.rigHold[rig] },
		OMThreshold: func(rig string) (float64, error) {
			f.omThresholdReads[rig]++
			if err := f.omThresholdErr[rig]; err != nil {
				return 0, err
			}
			return f.omThreshold[rig], nil
		},
		RevertInFlight: func(rig string) *specdispatch.Revert { return f.revert[rig] },
		Target:         func(rig string) *specdispatch.TargetTree { return f.targets[rig] },
		BranchOnOrigin: func(rig, branch string) (bool, error) {
			if f.originErr != nil {
				return false, f.originErr
			}
			return f.originBranches[rig+"/"+branch], nil
		},
		FormulaExists: func(_, name string) error {
			if f.missingFormulas[name] {
				return errors.New("formula not found")
			}
			return nil
		},
		Roster: func() (specRoster, error) { return f.roster, f.rosterErr },
		Annotate: func(id, key, text string) error {
			for _, n := range f.notes[id] {
				if strings.HasPrefix(n, key) {
					return nil
				}
			}
			f.notes[id] = append(f.notes[id], text)
			return nil
		},
		AddLabel: func(id, label string) error {
			f.labels[id] = append(f.labels[id], label)
			s := f.specs[id]
			s.Labels = append(s.Labels, label)
			f.specs[id] = s
			return nil
		},
		Sling: func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if errs := f.slingErrs[c.Spec.ID]; len(errs) > 0 {
				f.slingErrs[c.Spec.ID] = errs[1:]
				if errs[0] != nil {
					return "", errs[0]
				}
			}
			// The tick's slings run concurrently; the record keeps candidate
			// order through c.order so the assertions on it stay exact.
			f.slingRecords = append(f.slingRecords, specSlingRecord{order: c.order, id: c.Spec.ID, seat: seat, resume: resumeBranch, run: c.Run})
			sort.SliceStable(f.slingRecords, func(i, j int) bool { return f.slingRecords[i].order < f.slingRecords[j].order })
			f.slung, f.slingSeats, f.slungResume, f.slungRuns = nil, nil, nil, nil
			for _, rec := range f.slingRecords {
				f.slung = append(f.slung, rec.id)
				f.slingSeats = append(f.slingSeats, rec.seat)
				f.slungResume = append(f.slungResume, rec.resume)
				f.slungRuns = append(f.slungRuns, rec.run)
			}
			s := f.specs[c.Spec.ID]
			s.Status, s.Assignee = "hooked", "gastown/polecats/p"
			f.specs[c.Spec.ID] = s
			return "p", nil
		},
		Sleep:    func(time.Duration) { f.mu.Lock(); f.sleeps++; f.mu.Unlock() },
		Now:      func() time.Time { return specTestNow },
		Template: specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
		Budget:   budget,
		PerTick:  1,
		// The strict gate, so a test that cares about a refusal says so with
		// its fixture rather than by opting out of the town's default (warn).
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateRefuse,
	}
}

func TestSpecDispatchSlingsInDeterministicOrder(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-late", 2, "2026-09-29T12:00:00Z"),
		cleanSpec("gt-urgent", 1, "2026-09-29T13:00:00Z"),
		cleanSpec("gt-early", 2, "2026-09-29T10:00:00Z"),
	)
	env := f.env()
	env.PerTick = 3
	r := runSpecDispatchCycle(env)
	if got := strings.Join(f.slung, " "); got != "gt-urgent gt-early gt-late" {
		t.Fatalf("slung %q, want priority then created order", got)
	}
	if len(r.Dispatched) != 3 {
		t.Fatalf("report = %+v", r)
	}
	// The seat draw is random among the mix's free seats, so the agents are
	// not fixed; no seat may exceed its cap, and every sling takes one.
	counts := map[string]int{}
	for _, seat := range f.slingSeats {
		counts[seat.Agent]++
	}
	if counts["deepseek-flash"]+counts["claude-sonnet"] != 3 {
		t.Errorf("seats = %+v, want three slings on the mix's agents", f.slingSeats)
	}
	for agent, n := range counts {
		if n > 2 {
			t.Errorf("seat %s took %d slings, past its cap of 2", agent, n)
		}
	}
}

// The shape gate is the operator's (polecat_pool.shape_gate, gt-cq5gb): warn
// holds a bead the lint refuses and leaves the verdict on it once, refuse also
// labels the held bead needs-shape, off runs no lint at all, and a bead the
// operator waived with spec-shape-waived dispatches under any gate (gt-f8ppx).
func TestSpecDispatchShapeGate(t *testing.T) {
	t.Parallel()
	shapeless := func() specdispatch.Spec {
		s := cleanSpec("gt-bad", 1, "2026-09-29T10:00:00Z")
		s.Description = strings.Replace(s.Description, "## Gate\nmake gate", "", 1)
		return s
	}
	const refusedLine = "gt-bad: spec lint refused: ## Gate: section missing"

	t.Run("warn holds and notes", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(shapeless())
		env := f.env()
		env.ShapeGate = specdispatch.ShapeGateWarn
		r := runSpecDispatchCycle(env)
		if len(f.slung) != 0 || len(r.Dispatched) != 0 {
			t.Fatalf("warn slung a refused bead: slung %v report %+v", f.slung, r)
		}
		if len(r.Skipped) != 1 || r.Skipped[0].Line != "gt-bad: unshaped: ## Gate" {
			t.Fatalf("skipped = %+v, want one unshaped hold", r.Skipped)
		}
		if len(f.labels["gt-bad"]) != 0 {
			t.Errorf("warn must not label the bead: %v", f.labels["gt-bad"])
		}
		if n := f.notes["gt-bad"]; len(n) != 1 || !strings.HasPrefix(n[0], "SHAPE: ## Gate: section missing") {
			t.Errorf("notes = %v, want one SHAPE note", n)
		}
		// A second tick holds and reports the bead again, but writes no second
		// note for the same verdict.
		r = runSpecDispatchCycle(env)
		if len(r.Skipped) != 1 {
			t.Fatalf("second tick skipped = %+v", r.Skipped)
		}
		if n := f.notes["gt-bad"]; len(n) != 1 {
			t.Errorf("verdict noted %d times, want once", len(n))
		}
	})

	t.Run("waived slings despite the refusal", func(t *testing.T) {
		t.Parallel()
		for _, gate := range []string{specdispatch.ShapeGateWarn, specdispatch.ShapeGateRefuse} {
			waived := shapeless()
			waived.Labels = []string{specdispatch.ShapeWaivedLabel}
			f := newFakeSpecTown(waived)
			env := f.env()
			env.ShapeGate = gate
			r := runSpecDispatchCycle(env)
			if len(f.slung) != 1 || len(r.Dispatched) != 1 || len(r.Skipped) != 0 || len(r.Refused) != 0 {
				t.Fatalf("%s gate held a waived bead: slung %v report %+v", gate, f.slung, r)
			}
			if len(f.labels["gt-bad"]) != 0 {
				t.Errorf("%s gate labeled a waived bead: %v", gate, f.labels["gt-bad"])
			}
			if n := f.notes["gt-bad"]; len(n) != 1 || !strings.HasPrefix(n[0], "SHAPE: ## Gate: section missing") {
				t.Errorf("%s gate notes = %v, want one SHAPE note", gate, n)
			}
			if !strings.Contains(r.Dispatched[0].Line, "SHAPE: ## Gate: section missing") {
				t.Errorf("%s dispatch line %q must carry the verdict", gate, r.Dispatched[0].Line)
			}
		}
	})

	t.Run("refuse skips and labels", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(shapeless())
		r := runSpecDispatchCycle(f.env()) // the fixture's gate is refuse
		if len(f.slung) != 0 || len(r.Refused) != 1 || r.Refused[0].Line != refusedLine {
			t.Fatalf("slung %v refused %+v", f.slung, r.Refused)
		}
		if got := f.labels["gt-bad"]; len(got) != 1 || got[0] != "needs-shape" {
			t.Errorf("labels = %v, want needs-shape once", got)
		}
		if n := f.notes["gt-bad"]; len(n) != 1 || !strings.HasPrefix(n[0], "SHAPE: ## Gate: section missing") {
			t.Errorf("notes = %v, want one SHAPE note", n)
		}
	})

	t.Run("off runs no lint", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(shapeless())
		env := f.env()
		env.ShapeGate = specdispatch.ShapeGateOff
		r := runSpecDispatchCycle(env)
		if len(f.slung) != 1 || len(r.Refused) != 0 || len(f.notes) != 0 || len(f.labels) != 0 {
			t.Fatalf("off gate still gated: slung %v report %+v notes %v labels %v", f.slung, r, f.notes, f.labels)
		}
	})
}

// A held unshaped bead spends no seat and does not block the shaped beads
// behind it: the hold is a skip, not a stop (gt-f8ppx acceptance 4).
func TestSpecDispatchUnshapedHoldSpendsNoSeat(t *testing.T) {
	t.Parallel()
	bad := cleanSpec("gt-bad", 1, "2026-09-29T10:00:00Z")
	bad.Description = strings.Replace(bad.Description, "## Gate\nmake gate", "", 1)
	good := cleanSpec("gt-good", 2, "2026-09-29T11:00:00Z")
	f := newFakeSpecTown(bad, good)
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.PerTick = 1 // the held bead must not spend the one slot
	r := runSpecDispatchCycle(env)
	if got := strings.Join(f.slung, " "); got != "gt-good" {
		t.Fatalf("slung %q, want only the shaped bead", got)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Bead != "gt-bad" {
		t.Fatalf("skipped = %+v, want the unshaped bead held", r.Skipped)
	}
	if len(r.Dispatched) != 1 || r.Dispatched[0].Bead != "gt-good" {
		t.Fatalf("dispatched = %+v, want the shaped bead behind the hold", r.Dispatched)
	}
}

// A bead parked for a person or by a ruling never reaches the sling: the
// dispatcher reads the shared hold rule over the fresh bead and holds it out of
// the candidate set, so no tick spends a sling the guard then refuses
// (gt-lxxo4). The intake is a filter, so the drop is silent — the assertion
// that matters is that no attempt was made.
func TestSpecDispatchHoldsParkedBeads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*specdispatch.Spec)
	}{
		// The spelling the landing worker writes, and the one the ready args'
		// exclude list never carried.
		{"gt:needs-human", func(s *specdispatch.Spec) { s.Labels = []string{"gt:needs-human"} }},
		{"operator", func(s *specdispatch.Spec) { s.Labels = []string{"operator"} }},
		{"design ruling", func(s *specdispatch.Spec) { s.Design = "do not redispatch: park it." }},
		{"notes ruling", func(s *specdispatch.Spec) { s.Notes = "do not redispatch" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
			tc.edit(&s)
			f := newFakeSpecTown(s)
			r := runSpecDispatchCycle(f.env())
			if len(f.slung) != 0 || len(r.Dispatched) != 0 || len(r.Skipped) != 0 {
				t.Fatalf("a parked bead was taken: slung %v skipped %+v report %+v", f.slung, r.Skipped, r)
			}
		})
	}
}

// The control: the same tick still slings a bead that carries no hold, so the
// cases above are not passing because the tick did nothing at all.
func TestSpecDispatchSlingsAnUnheldBead(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slung[0] != "gt-a" || len(r.Dispatched) != 1 {
		t.Fatalf("slung %v report %+v, want the unheld bead taken", f.slung, r)
	}
}

// needs-pro was the pro seat's selector. The reserved seat is retired, so the
// shared rule's hold on the label stands and the tick dispatches nothing
// (gt-lxxo4).
func TestSpecDispatchHoldsNeedsPro(t *testing.T) {
	t.Parallel()
	pro := cleanSpec("gt-pro", 1, "2026-09-29T10:00:00Z")
	pro.Labels = []string{"needs-pro"}

	f := newFakeSpecTown(pro)
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Dispatched) != 0 {
		t.Fatalf("slung %v report %+v, want a needs-pro bead held", f.slung, r)
	}
}

// A bead whose children are open is a container, so it is held under any gate
// and spends no seat; one whose children have all closed is a unit of work
// again (gt-gektq).
func TestSpecDispatchOpenChildrenHoldTheContainer(t *testing.T) {
	t.Parallel()
	container := cleanSpec("gt-container", 1, "2026-09-29T10:00:00Z")
	landed := cleanSpec("gt-landed", 2, "2026-09-29T11:00:00Z")

	t.Run("open child holds, under the off gate too", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(container)
		f.children["gt-container"] = []specdispatch.Child{
			{ID: "gt-container.1", Status: "open"},
			{ID: "gt-container.2", Status: "closed"},
		}
		env := f.env()
		env.ShapeGate = specdispatch.ShapeGateOff // shape is no gate; the container rule still is
		r := runSpecDispatchCycle(env)
		if len(f.slung) != 0 || len(r.Dispatched) != 0 {
			t.Fatalf("slung a container: slung %v report %+v", f.slung, r)
		}
		want := "gt-container: held: container: open child gt-container.1"
		if len(r.Skipped) != 1 || r.Skipped[0].Line != want {
			t.Fatalf("skipped = %+v, want %q", r.Skipped, want)
		}
	})

	t.Run("all children closed slings", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(container, landed)
		f.children["gt-container"] = []specdispatch.Child{
			{ID: "gt-container.1", Status: "closed"},
			{ID: "gt-container.2", Status: "tombstone"},
		}
		r := runSpecDispatchCycle(f.env())
		if got := strings.Join(f.slung, " "); got != "gt-container" {
			t.Fatalf("slung %q, want the container whose children all landed", got)
		}
		if len(r.Dispatched) != 1 || r.Dispatched[0].Bead != "gt-container" {
			t.Fatalf("dispatched = %+v", r.Dispatched)
		}
	})

	t.Run("an unreadable child set holds", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(container)
		f.childErrs["gt-container"] = errors.New("bd children: boom")
		r := runSpecDispatchCycle(f.env())
		if len(f.slung) != 0 {
			t.Fatalf("slung a bead whose children could not be read: %v", f.slung)
		}
		if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "cannot read children") {
			t.Fatalf("errors = %+v, want one children-read failure", r.Errors)
		}
	})

	// containerFromRow draws the candidate from a ready-board row, so the two
	// cases below run on what the board actually hands the tick: a row whose
	// dependent_count is zero because bd counts blocks edges only (gt-603wu).
	containerFromRow := func(t *testing.T, dependentCount int) specdispatch.Spec {
		t.Helper()
		desc, err := json.Marshal(specTestDescription)
		if err != nil {
			t.Fatal(err)
		}
		accept, err := json.Marshal(specTestAcceptance)
		if err != nil {
			t.Fatal(err)
		}
		rows := fmt.Sprintf(`[{"id":"gt-container","issue_type":"task","status":"open","priority":1,`+
			`"created_at":"2026-09-29T10:00:00Z","description":%s,"acceptance_criteria":%s,"dependent_count":%d}]`,
			desc, accept, dependentCount)
		issues, _, err := beads.DecodeReadyBoard([]byte(rows))
		if err != nil || len(issues) != 1 {
			t.Fatalf("decode board: %d issues, %v", len(issues), err)
		}
		return specFromIssue(issues[0])
	}

	t.Run("a row that counts no dependents still reads its children", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(containerFromRow(t, 0))
		f.children["gt-container"] = []specdispatch.Child{{ID: "gt-container.1", Status: "open"}}
		r := runSpecDispatchCycle(f.env())
		if len(f.slung) != 0 || len(r.Dispatched) != 0 {
			t.Fatalf("slung a parent the board row counted childless: slung %v report %+v", f.slung, r)
		}
		want := "gt-container: held: container: open child gt-container.1"
		if len(r.Skipped) != 1 || r.Skipped[0].Line != want {
			t.Fatalf("skipped = %+v, want %q", r.Skipped, want)
		}
	})

	t.Run("a row that counts no dependents still holds an unreadable child set", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(containerFromRow(t, 0))
		f.childErrs["gt-container"] = errors.New("bd children: boom")
		r := runSpecDispatchCycle(f.env())
		if len(f.slung) != 0 {
			t.Fatalf("slung a bead whose children could not be read: %v", f.slung)
		}
		if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "cannot read children") {
			t.Fatalf("errors = %+v, want one children-read failure", r.Errors)
		}
	})
}

// specHoldFiles drops a cited claude/docs standard once the bead's Constraints
// section parses, listed or not: the standards tree lives in ~/.claude and no
// polecat edits it (gt-kr17g). A section that is missing, empty or unparsed
// leaves the bead's paths unread, so the doc stays in the hold set (gt-zs8fq).
func TestSpecHoldFilesDropsDocOnlyWhenConstraintsParse(t *testing.T) {
	t.Parallel()
	const standard = "claude/docs/standards/test-engineering.md"
	cases := []struct {
		name        string
		description string
		want        []string
	}{
		{"no Constraints section", "## Goal\nA thing.", []string{standard}},
		{"empty Constraints section", "## Goal\nA thing.\n\n## Constraints\n", []string{standard}},
		{"unparsed Constraints heading", "## Goal\nA thing.\n\n## Constraint:\nGo.", []string{standard}},
		{"Constraints does not list the doc", "## Goal\nA thing.\n\n## Constraints\nGo.", []string{}},
		{"Constraints lists the standard", "## Goal\nA thing.\n\n## Constraints\nUpdate " + standard + ".", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := specHoldFiles([]string{standard}, c.description, c.description); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("specHoldFiles = %v, want %v", got, c.want)
			}
		})
	}
}

// A Constraints section that names no path leaves the files a follow-up changes
// unstated, so the reference doc its Goal names is the work and stays held. A
// Constraints section that names other paths keeps the rule that a cited doc is
// read, not changed (gt-1e5s8).
// A bead with no Goal, or an empty one, leaves that work unknown too, so the doc
// stays held (gt-zs8fq).
func TestSpecHoldFilesKeepsGoalDocsWhenConstraintsNameNoPath(t *testing.T) {
	t.Parallel()
	const doc = "docs/reference.md"
	cases := []struct {
		name        string
		description string
		want        []string
	}{
		{"Constraints names no path", "## Goal\nRewrite " + doc + ".\n\n## Constraints\nKeep the gate green.", []string{doc}},
		{"Goal does not name the doc", "## Goal\nA thing.\n\n## Constraints\nKeep the gate green.", []string{}},
		{"Constraints names another path", "## Goal\nRewrite " + doc + ".\n\n## Constraints\nChange only internal/cmd/spec.go.", []string{}},
		{"Goal section is missing", "## Constraints\nKeep the gate green.", []string{doc}},
		{"Goal section is empty", "## Goal\n\n## Constraints\nKeep the gate green.", []string{doc}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := specHoldFiles([]string{doc}, c.description, c.description); !reflect.DeepEqual(got, c.want) {
				t.Errorf("specHoldFiles = %v, want %v", got, c.want)
			}
		})
	}
}

// --dry-run holds an unshaped bead too: it reports the unshaped reason and
// writes nothing (gt-f8ppx acceptance 1).
func TestSpecDispatchDryRunHoldsUnshaped(t *testing.T) {
	t.Parallel()
	bad := cleanSpec("gt-bad", 1, "2026-09-29T10:00:00Z")
	bad.Description = strings.Replace(bad.Description, "## Gate\nmake gate", "", 1)
	f := newFakeSpecTown(bad)
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.DryRun = true
	r := runSpecDispatchCycle(env)
	if len(r.Skipped) != 1 || r.Skipped[0].Line != "gt-bad: unshaped: ## Gate" {
		t.Fatalf("skipped = %+v, want the unshaped hold", r.Skipped)
	}
	if len(f.slung) != 0 || len(f.notes) != 0 || len(f.labels) != 0 {
		t.Fatalf("dry run had side effects: slung %v notes %v labels %v", f.slung, f.notes, f.labels)
	}
}

func TestSpecDispatchPerTickLimit(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T10:00:00Z"), cleanSpec("gt-b", 2, "2026-09-29T11:00:00Z"))
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slung[0] != "gt-a" || len(r.Skipped) != 1 {
		t.Fatalf("slung %v skipped %+v", f.slung, r.Skipped)
	}
}

func TestSpecDispatchRefusesAndAnnotatesOnce(t *testing.T) {
	t.Parallel()
	bad := cleanSpec("gt-bad", 1, "2026-09-29T10:00:00Z")
	bad.Description = strings.Replace(bad.Description, "## Gate\nmake gate", "", 1)
	f := newFakeSpecTown(bad)
	for i := 0; i < 3; i++ {
		r := runSpecDispatchCycle(f.env())
		if len(r.Refused) != 1 || r.Refused[0].Line != "gt-bad: spec lint refused: ## Gate: section missing" {
			t.Fatalf("tick %d: refused = %+v", i, r.Refused)
		}
	}
	if len(f.slung) != 0 {
		t.Fatalf("refused spec was slung: %v", f.slung)
	}
	if n := len(f.notes["gt-bad"]); n != 1 {
		t.Fatalf("annotated %d times, want once: %v", n, f.notes["gt-bad"])
	}
}

func TestSpecDispatchRoutesPlanningWithoutSpawning(t *testing.T) {
	t.Parallel()
	big := cleanSpec("gt-big", 1, "2026-09-29T10:00:00Z")
	big.Acceptance = strings.Repeat("- [ ] x\n", 8)
	labeled := cleanSpec("gt-plan", 1, "2026-09-29T11:00:00Z")
	labeled.Labels = append(labeled.Labels, "needs-planning")
	f := newFakeSpecTown(big, labeled)
	r := runSpecDispatchCycle(f.env())
	runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Planning) != 2 {
		t.Fatalf("slung %v planning %+v", f.slung, r.Planning)
	}
	if got := f.labels["gt-big"]; len(got) != 1 || got[0] != "needs-planning" {
		t.Errorf("oversized spec labels added = %v, want needs-planning once", got)
	}
	if got := f.labels["gt-plan"]; len(got) != 0 {
		t.Errorf("already-labeled spec relabeled: %v", got)
	}
	if len(f.notes["gt-big"]) != 1 || len(f.notes["gt-plan"]) != 1 {
		t.Errorf("planning notes = %v", f.notes)
	}
}

// The default warn gate holds a spec that needs planning just as it holds one
// the lint refuses (gt-tod3q): the planning spec is never slung, wears
// needs-planning, and is commented by its planning state, while a shape-refused
// spec keeps warn's old behaviour — held with one SHAPE comment and no label —
// and a clean spec behind both still takes its seat.
func TestSpecDispatchWarnGateHoldsPlanning(t *testing.T) {
	t.Parallel()
	big := cleanSpec("gt-big", 1, "2026-09-29T10:00:00Z")
	big.Acceptance = strings.Repeat("- [ ] x\n", 8)
	bad := cleanSpec("gt-bad", 2, "2026-09-29T10:30:00Z")
	bad.Description = strings.Replace(bad.Description, "## Gate\nmake gate", "", 1)
	good := cleanSpec("gt-good", 2, "2026-09-29T11:00:00Z")
	f := newFakeSpecTown(big, bad, good)
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.PerTick = 2 // the two holds must not spend a seat
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-good" {
		t.Fatalf("slung %q, want only the clean bead", got)
	}
	if len(r.Planning) != 1 || r.Planning[0].Bead != "gt-big" || !strings.Contains(r.Planning[0].Line, specPlanNeedsPlan) {
		t.Fatalf("planning = %+v, want gt-big held for a plan", r.Planning)
	}
	if got := f.labels["gt-big"]; len(got) != 1 || got[0] != specdispatch.NeedsPlanningLabel {
		t.Errorf("planning labels = %v, want needs-planning once", got)
	}
	if n := f.notes["gt-big"]; len(n) != 1 || !strings.HasPrefix(n[0], specDispatchNotePrefix+"gt-big: "+specPlanNeedsPlan) {
		t.Errorf("planning notes = %v, want one planning note", n)
	}
	if got := f.labels["gt-bad"]; len(got) != 0 {
		t.Errorf("warn labeled a shape-refused bead: %v", got)
	}
	if n := f.notes["gt-bad"]; len(n) != 1 || !strings.HasPrefix(n[0], "SHAPE: ## Gate: section missing") {
		t.Errorf("shape notes = %v, want one SHAPE note", n)
	}
	// A second tick reports the planning bead again without a second note.
	r = runSpecDispatchCycle(env)
	if len(r.Planning) != 1 || len(f.notes["gt-big"]) != 1 || len(f.notes["gt-bad"]) != 1 {
		t.Errorf("second tick planning = %+v notes %v", r.Planning, f.notes)
	}
}

// TestSpecDispatchReportsPlanState: the tick says which of the two planning
// states a spec is in — waiting for a plan job's proposal, or carrying one the
// operator files — instead of naming a planner that is not built (gt-4k3fj.14).
func TestSpecDispatchReportsPlanState(t *testing.T) {
	t.Parallel()
	waiting := cleanSpec("gt-wait", 1, "2026-09-29T10:00:00Z")
	waiting.Labels = append(waiting.Labels, "needs-planning")
	proposed := cleanSpec("gt-done", 1, "2026-09-29T11:00:00Z")
	proposed.Labels = append(proposed.Labels, "needs-planning")
	proposed.Notes = specdispatch.PlanProposalMarker + "\nSpec: gt-done\nChildren: 2\n"
	f := newFakeSpecTown(waiting, proposed)
	r := runSpecDispatchCycle(f.env())
	if len(r.Planning) != 2 {
		t.Fatalf("planning = %+v, want both specs reported", r.Planning)
	}
	line := map[string]string{}
	for _, e := range r.Planning {
		line[e.Bead] = e.Line
	}
	if !strings.Contains(line["gt-wait"], "needs plan") {
		t.Errorf("gt-wait line = %q, want the needs-plan state", line["gt-wait"])
	}
	if !strings.Contains(line["gt-done"], "plan proposed") {
		t.Errorf("gt-done line = %q, want the plan-proposed state", line["gt-done"])
	}
	for _, l := range line {
		if strings.Contains(l, "gt-4k3fj.7") || strings.Contains(l, "not built") {
			t.Errorf("line %q still reports a planner that is not built", l)
		}
	}
	// The two states leave two different notes, so the bead's history says
	// when the proposal arrived.
	if len(f.notes["gt-wait"]) != 1 || !strings.HasPrefix(f.notes["gt-wait"][0], specDispatchNotePrefix+"gt-wait: needs plan") {
		t.Errorf("gt-wait notes = %v", f.notes["gt-wait"])
	}
	if len(f.notes["gt-done"]) != 1 || !strings.HasPrefix(f.notes["gt-done"][0], specDispatchNotePrefix+"gt-done: plan proposed") {
		t.Errorf("gt-done notes = %v", f.notes["gt-done"])
	}
}

func TestSpecSlingParams(t *testing.T) {
	t.Parallel()
	c := specCandidate{Spec: cleanSpec("gt-a", 1, ""), Rig: "gastown"}
	p := specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", c, "", specdispatch.SeatChoice{Agent: "claude-sonnet"})
	if p.Agent != "claude-sonnet" || !p.NoBoot || !p.FormulaFailFatal ||
		p.RigName != "gastown" || p.FormulaName != "mol-polecat-work" || !strings.Contains(p.Args, "temporary INSTALL_DIR") {
		t.Fatalf("params = %+v", p)
	}
	if p.ResumeBranch != "" {
		t.Errorf("ResumeBranch = %q, want empty for a bead with no recorded branch", p.ResumeBranch)
	}
	// The resume branch is the gt sling --branch the recovered work continues on.
	p = specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", c, "polecat/mica/gt-a+mu1", specdispatch.SeatChoice{Agent: "claude-sonnet"})
	if p.ResumeBranch != "polecat/mica/gt-a+mu1" {
		t.Errorf("ResumeBranch = %q, want the recorded branch", p.ResumeBranch)
	}
}

// formulaSpecBead is a clean spec that declares its formula (gt-j5cr2).
func formulaSpecBead(id string, labels []string, vars string) specdispatch.Spec {
	s := cleanSpec(id, 1, "2026-09-29T10:00:00Z")
	s.Labels = labels
	if vars != "" {
		s.Description += "\n\n## Formula vars\n" + vars
	}
	return s
}

// TestSpecDispatchSlingsADeclaredFormula: a bead labelled formula:<name> is
// slung with that formula and its vars, a scheduled one under the scheduler's
// actor, and a bead without the label is slung exactly as before (gt-j5cr2).
func TestSpecDispatchSlingsADeclaredFormula(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		formulaSpecBead("gt-plain", []string{"formula:mol-deep-review"}, "scope=all\n# note\nrig=gastown"),
		func() specdispatch.Spec {
			s := formulaSpecBead("gt-sched", []string{"formula:mol-deep-review", "scheduled:deep-review"}, "scope=week")
			s.CreatedAt = "2026-09-29T11:00:00Z"
			return s
		}(),
		func() specdispatch.Spec {
			s := cleanSpec("gt-none", 1, "2026-09-29T12:00:00Z")
			s.Description += "\n\n## Formula vars\nignored=without-a-label"
			return s
		}(),
	)
	env := f.env()
	env.PerTick = 3
	r := runSpecDispatchCycle(env)
	if got := strings.Join(f.slung, " "); got != "gt-plain gt-sched gt-none" {
		t.Fatalf("slung %q; report = %+v", got, r)
	}
	want := []specdispatch.FormulaRun{
		{Formula: "mol-deep-review", Vars: []string{"scope=all", "rig=gastown"}},
		{Formula: "mol-deep-review", Vars: []string{"scope=week"}, Actor: "daemon/scheduled:deep-review"},
		{},
	}
	if !reflect.DeepEqual(f.slungRuns, want) {
		t.Errorf("runs = %+v, want %+v", f.slungRuns, want)
	}
	if !strings.Contains(r.Dispatched[0].Line, ", formula mol-deep-review") || strings.Contains(r.Dispatched[2].Line, "formula") {
		t.Errorf("lines = %q", []string{r.Dispatched[0].Line, r.Dispatched[2].Line})
	}

	// The params the production Sling closure builds carry them to the engine.
	p := specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", specCandidate{Spec: f.specs["gt-sched"], Rig: "gastown", Run: want[1]}, "", specdispatch.SeatChoice{Agent: "claude-sonnet"})
	if p.FormulaName != "mol-deep-review" || p.Actor != "daemon/scheduled:deep-review" || !reflect.DeepEqual(p.Vars, []string{"scope=week"}) ||
		!strings.Contains(p.Args, "temporary INSTALL_DIR") || !p.FormulaFailFatal {
		t.Errorf("params = %+v", p)
	}
	p = specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", specCandidate{Spec: f.specs["gt-none"], Rig: "gastown"}, "", specdispatch.SeatChoice{Agent: "claude-sonnet"})
	if p.FormulaName != "mol-polecat-work" || p.Actor != "" || len(p.Vars) != 0 {
		t.Errorf("unlabelled params = %+v", p)
	}
}

// TestSpecDispatchSkipsABadFormulaDeclaration: a declaration the tick cannot
// read, or a formula the rig lacks, is a skip with its reason; the bead is not
// slung, not labelled failed, and stays ready (gt-j5cr2).
func TestSpecDispatchSkipsABadFormulaDeclaration(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		formulaSpecBead("gt-badvars", []string{"formula:mol-deep-review"}, "scope=all\noops"),
		formulaSpecBead("gt-nokey", []string{"formula:mol-deep-review"}, "=x"),
		formulaSpecBead("gt-nofm", []string{"formula:mol-gone"}, "a=1"),
	)
	f.missingFormulas = map[string]bool{"mol-gone": true}
	env := f.env()
	env.PerTick = 3
	r := runSpecDispatchCycle(env)
	if len(f.slung) != 0 || len(r.Dispatched) != 0 || len(r.Failed) != 0 {
		t.Fatalf("slung %v; report = %+v", f.slung, r)
	}
	lines := map[string]string{}
	for _, e := range r.Skipped {
		lines[e.Bead] = e.Line
	}
	for id, want := range map[string]string{
		"gt-badvars": "gt-badvars: bad formula vars: oops",
		"gt-nokey":   "gt-nokey: bad formula vars: =x",
		"gt-nofm":    "gt-nofm: unknown formula: mol-gone",
	} {
		if lines[id] != want {
			t.Errorf("%s skip = %q, want %q", id, lines[id], want)
		}
		if len(f.labels[id]) != 0 {
			t.Errorf("%s labelled %v, want none: a skipped bead is never lost", id, f.labels[id])
		}
	}
}

// TestSpecDispatchDryRunShowsTheFormula: the dry run names the formula beside
// the seat (gt-j5cr2).
func TestSpecDispatchDryRunShowsTheFormula(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(formulaSpecBead("gt-a", []string{"formula:mol-deep-review"}, "k=v"))
	env := f.env()
	env.DryRun = true
	r := runSpecDispatchCycle(env)
	if len(r.Dispatched) != 1 || len(f.slung) != 0 {
		t.Fatalf("slung %v; report = %+v", f.slung, r)
	}
	// The seat draw is random among the mix's free seats, so the line's agent
	// is not fixed; it names the agent, its seat and the formula.
	if got := r.Dispatched[0].Line; !strings.HasPrefix(got, "gt-a: would sling to gastown on ") ||
		!strings.Contains(got, " (seat ") || !strings.HasSuffix(got, "), formula mol-deep-review") {
		t.Errorf("line = %q", got)
	}
}

// TestSpecDispatchResumesARecordedBranch: a bead the patrol tick readied after
// its polecat died is slung onto the branch its work survives on, and the tick
// line names it (gt-gzhin.3).
func TestSpecDispatchResumesARecordedBranch(t *testing.T) {
	t.Parallel()
	const branch = "polecat/mica/gt-a+mu1"
	recovered := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	recovered.Notes = "some note\n" + patrolscan.ResumeBranchNote(branch) + "\n"
	f := newFakeSpecTown(recovered)
	f.originBranches["gastown/"+branch] = true

	r := runSpecDispatchCycle(f.env())
	if got := strings.Join(f.slungResume, "|"); got != branch {
		t.Fatalf("slung resume branches = %q, want %q (slung %v)", got, branch, f.slung)
	}
	if len(r.Dispatched) != 1 || !strings.Contains(r.Dispatched[0].Line, "resumed "+branch) {
		t.Fatalf("dispatched = %+v, want the line to name the resumed branch", r.Dispatched)
	}
	if n := f.notes["gt-a"]; len(n) != 0 {
		t.Errorf("notes = %v, want none: the branch exists, so nothing was lost", n)
	}
}

// TestSpecDispatchSlingsFreshWhenTheBranchIsGone: a recorded branch that no
// longer exists on origin cannot be resumed; the bead is slung fresh and a
// comment records the loss (gt-gzhin.3).
func TestSpecDispatchSlingsFreshWhenTheBranchIsGone(t *testing.T) {
	t.Parallel()
	const branch = "polecat/mica/gt-a+mu1"
	recovered := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	recovered.Notes = patrolscan.ResumeBranchNote(branch)
	f := newFakeSpecTown(recovered)

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slungResume[0] != "" {
		t.Fatalf("slung %v resume %v, want a fresh sling", f.slung, f.slungResume)
	}
	if len(r.Dispatched) != 1 || !strings.Contains(r.Dispatched[0].Line, "resume branch "+branch+" gone") {
		t.Fatalf("dispatched = %+v, want the line to name the gone branch", r.Dispatched)
	}
	if n := f.notes["gt-a"]; len(n) != 1 || !strings.Contains(n[0], branch) || !strings.Contains(n[0], "gone from origin") {
		t.Fatalf("notes = %v, want one comment recording the gone branch", n)
	}
	// The comment lands once, not once per tick.
	runSpecDispatchCycle(f.env())
	if n := f.notes["gt-a"]; len(n) != 1 {
		t.Errorf("notes = %v, want the gone comment written once", n)
	}
}

// TestSpecDispatchSlingsFreshWithoutAResumeLine: a bead with no resume_branch
// note dispatches exactly as before — a fresh sling and no comment.
func TestSpecDispatchSlingsFreshWithoutAResumeLine(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slungResume[0] != "" || len(f.notes) != 0 {
		t.Fatalf("slung %v resume %v notes %v, want a plain fresh sling", f.slung, f.slungResume, f.notes)
	}
	if len(r.Dispatched) != 1 || strings.Contains(r.Dispatched[0].Line, "resum") {
		t.Fatalf("dispatched = %+v, want no resume note", r.Dispatched)
	}
}

// TestSpecDispatchHoldsACandidateWhenOriginCannotBeChecked: a resume branch
// that cannot be verified is unknown, not gone, so the bead is left for the
// next tick instead of being handed to a polecat starting from main.
func TestSpecDispatchHoldsACandidateWhenOriginCannotBeChecked(t *testing.T) {
	t.Parallel()
	recovered := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	recovered.Notes = patrolscan.ResumeBranchNote("polecat/mica/gt-a+mu1")
	f := newFakeSpecTown(recovered)
	f.originErr = errors.New("origin unreachable")

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want none: the branch could not be checked", f.slung)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "cannot check resume branch") {
		t.Fatalf("report = %+v, want one error naming the uncheckable branch", r)
	}
}

func TestSpecDispatchRespectsCapsAndRoster(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.roster = specRoster{Live: map[string]int{"deepseek-flash": 2, "claude-sonnet": 2}}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || !strings.Contains(r.Skipped[0].Line, "seats full") {
		t.Fatalf("slung %v skipped %+v", f.slung, r.Skipped)
	}
	if r.Roster != "deepseek-flash 2/2, claude-sonnet 2/2" {
		t.Errorf("roster = %q", r.Roster)
	}

	f = newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.rosterErr = errors.New("no tmux server")
	r = runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Errors) != 1 {
		t.Fatalf("an unreadable roster must not dispatch: slung %v report %+v", f.slung, r)
	}

	f = newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.Budget.MinSpawnGap = 4 * time.Minute
	f.roster = specRoster{Newest: env.Now().Add(-time.Minute)}
	if runSpecDispatchCycle(env); len(f.slung) != 0 {
		t.Fatalf("min_spawn_gap ignored: slung %v", f.slung)
	}
}

// gt-zkdwt: the dispatcher slung a red-main bead while the red-main owner's
// revert of the same breakage was in flight, sending a polecat to fix forward
// in parallel with the revert that supersedes it.
func TestSpecDispatchHoldsRedMainBeadsWhileARevertIsInFlight(t *testing.T) {
	t.Parallel()
	redMain := cleanSpec("gt-red", 1, "2026-09-29T10:00:00Z")
	redMain.Labels = []string{specdispatch.LabelRedMain}

	// The revert is building or queued: the fix-forward bead is not a
	// candidate, and nothing is written on it.
	f := newFakeSpecTown(redMain)
	f.revert["gastown"] = &specdispatch.Revert{Culprit: "gt-cul", StartedAt: specTestNow.Add(-5 * time.Minute)}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 {
		t.Fatalf("slung the red-main bead while a revert was in flight: %v", f.slung)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "gt-cul") {
		t.Fatalf("report = %+v; want one skip naming the culprit", r)
	}
	if len(f.notes["gt-red"]) != 0 || len(f.labels["gt-red"]) != 0 {
		t.Fatalf("held bead commented or labeled: notes %v, labels %v", f.notes, f.labels)
	}

	// A landed, rejected or never-filed revert leaves no state, and the bead
	// is a candidate again.
	f = newFakeSpecTown(redMain)
	if r := runSpecDispatchCycle(f.env()); len(f.slung) != 1 {
		t.Fatalf("slung %v, want the red-main bead once no revert is in flight (report %+v)", f.slung, r)
	}
}

// The hold is the red-main bead's alone: other work in the rig keeps
// dispatching while the owner reverts.
func TestSpecDispatchHoldsOnlyRedMainBeads(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-plain", 1, "2026-09-29T10:00:00Z"))
	f.revert["gastown"] = &specdispatch.Revert{Culprit: "gt-cul", StartedAt: specTestNow.Add(-5 * time.Minute)}
	if r := runSpecDispatchCycle(f.env()); len(f.slung) != 1 {
		t.Fatalf("slung %v, want the plain bead dispatched beside a revert in flight (report %+v)", f.slung, r)
	}
}

// gt-wgyca: a revert record left behind with no bead — a crash mid-build, or
// one closed by hand — held the rig's red-main beads with nothing to expire
// it. A bead-less record past the age is ignored, and the tick says so once,
// naming the culprit and how long it has sat.
func TestSpecDispatchIgnoresAStaleRevertRecord(t *testing.T) {
	t.Parallel()
	redMain := cleanSpec("gt-red", 1, "2026-09-29T10:00:00Z")
	redMain.Labels = []string{specdispatch.LabelRedMain}

	f := newFakeSpecTown(redMain)
	f.revert["gastown"] = &specdispatch.Revert{Culprit: "gt-cul", StartedAt: specTestNow.Add(-31 * time.Minute)}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 {
		t.Fatalf("slung %v, want the red-main bead once the record is stale (report %+v)", f.slung, r)
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("stale record still held a bead: %+v", r.Skipped)
	}
	if len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "gt-cul") || !strings.Contains(r.Notices[0], "31 minutes") {
		t.Fatalf("notices = %v, want one naming the culprit and its age", r.Notices)
	}

	// A record with no start time at all is the same stale: it cannot prove
	// the build it describes is still running.
	f = newFakeSpecTown(redMain)
	f.revert["gastown"] = &specdispatch.Revert{Culprit: "gt-cul"}
	r = runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "gt-cul") {
		t.Fatalf("slung %v notices %v; want the bead dispatched with one notice", f.slung, r.Notices)
	}
}

// The state read is the dispatcher's only view of the revert: absent,
// malformed and revert-free states all mean "no revert", so a rig whose state
// cannot be read never loses its red-main beads for good.
func TestRigRevertInFlightReadsTheStateFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "red-main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	open := func(string) bool { return true }
	silent := func(string) bool { return false }
	if rv := rigRevertInFlight(town, "gastown", open); rv != nil {
		t.Fatalf("absent state = %+v, want nil", rv)
	}
	state := filepath.Join(dir, "gastown.json")
	if err := os.WriteFile(state, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rv := rigRevertInFlight(town, "gastown", open); rv != nil {
		t.Fatalf("malformed state = %+v, want nil", rv)
	}
	if err := os.WriteFile(state, []byte(`{"last_green":"aaa","revert":{"culprit":"gt-cul","bead":"gt-rv"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rv := rigRevertInFlight(town, "gastown", open); rv == nil || rv.Culprit != "gt-cul" || rv.Bead != "gt-rv" {
		t.Fatalf("state = %+v, want the revert of gt-cul as gt-rv", rv)
	}
	// A closed or missing revert bead is a revert that already finished, or
	// one a crash never filed: the record is dead and holds nothing (gt-wgyca).
	if rv := rigRevertInFlight(town, "gastown", silent); rv != nil {
		t.Fatalf("closed or missing bead = %+v, want nil", rv)
	}
	// A building revert carries no bead, so no bead status can rule it out.
	if err := os.WriteFile(state, []byte(`{"revert":{"culprit":"gt-cul","started_at":"2026-09-30T11:59:00Z"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rv := rigRevertInFlight(town, "gastown", silent); rv == nil || rv.Culprit != "gt-cul" {
		t.Fatalf("building revert = %+v, want the revert of gt-cul", rv)
	}
}

// A revert bead holds only while it is open: a closed one is a revert that
// already landed or was rejected, and a dispatcher that kept holding on it
// would strand the rig's red-main beads (gt-wgyca).
func TestRevertStatusOpen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status string
		open   bool
	}{
		{status: "open", open: true},
		{status: "in_progress", open: true},
		{status: "hooked", open: true},
		{status: "closed"},
		{status: "tombstone"},
		{status: " closed "},
	} {
		if got := revertStatusOpen(tc.status); got != tc.open {
			t.Errorf("revertStatusOpen(%q) = %v, want %v", tc.status, got, tc.open)
		}
	}
}

func TestSpecDispatchHoldsAndExclusions(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.hold = "town ESTOP active"
	if r := runSpecDispatchCycle(f.env()); r.Hold == "" || len(f.slung) != 0 {
		t.Fatalf("hold ignored: %+v", r)
	}

	f = newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.rigHold["gastown"] = "rig ESTOP"
	if runSpecDispatchCycle(f.env()); len(f.slung) != 0 {
		t.Fatalf("rig hold ignored")
	}

	var excluded []specdispatch.Spec
	for i, label := range []string{"gt:ready-to-land", "needs-human"} {
		s := cleanSpec("gt-x"+string(rune('a'+i)), 1, "2026-09-29T10:00:00Z")
		s.Labels = append(s.Labels, label)
		excluded = append(excluded, s)
	}
	deferred := cleanSpec("gt-deferred", 1, "2026-09-29T10:00:00Z")
	deferred.Status = "deferred"
	excluded = append(excluded, deferred)
	f = newFakeSpecTown(excluded...)
	if r := runSpecDispatchCycle(f.env()); len(f.slung) != 0 || r.Candidates != 0 {
		t.Fatalf("excluded beads considered: slung %v report %+v", f.slung, r)
	}

	// The reviewer label was retired with the role (gt-rwp7z.11): it is on no
	// exclusion list and routes nothing, so a bead still wearing it dispatches
	// as ordinary ready work. An operator with held beads re-labels them.
	retired := cleanSpec("gt-retired-label", 1, "2026-09-29T10:00:00Z")
	retired.Labels = append(retired.Labels, "needs-mayor-review")
	f = newFakeSpecTown(retired)
	if r := runSpecDispatchCycle(f.env()); len(f.slung) != 1 {
		t.Fatalf("the retired label still held a bead: slung %v report %+v", f.slung, r)
	}
}

// A rig held out of new dispatches has its candidate skipped while another
// rig's bead in the same tick is dispatched: the tick reads the hold record
// off the registry (dispatch.RigHold), so the hold stops gastown and leaves
// devops alone (gt-vty1v acceptance 3).
func TestSpecDispatchHeldRigSkipsOnlyItsCandidates(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	data, err := json.Marshal(&config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{"gastown": {}, "devops": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := townconfig.Hold(townRoot, "gastown", config.RigDispatchHold{
		Since: specTestNow, By: "sloan", Reason: "drain the landing backlog",
	}); err != nil {
		t.Fatal(err)
	}

	f := newFakeSpecTown(
		cleanSpec("gt-held", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("dv-open", 1, "2026-09-29T11:00:00Z"),
	)
	f.rigOf = map[string]string{"gt-held": "gastown", "dv-open": "devops"}
	env := f.env()
	env.RigHold = func(rig string) string { return dispatch.RigHold(townRoot, rig) }
	env.PerTick = 2

	report := runSpecDispatchCycle(env)

	if len(f.slung) != 1 || f.slung[0] != "dv-open" {
		t.Errorf("slung = %v, want the devops candidate alone", f.slung)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].Bead != "gt-held" || report.Skipped[0].Rig != "gastown" {
		t.Fatalf("skipped = %+v, want the held gastown candidate alone", report.Skipped)
	}
	line := report.Skipped[0].Line
	if !strings.Contains(line, specdispatch.SkipRigHold) || !strings.Contains(line, "drain the landing backlog") ||
		!strings.Contains(line, "gt rig release gastown") {
		t.Errorf("skip line = %q, want a rig hold naming the reason and the release command", line)
	}
}

// A bead whose last sling failed at session start rests out its backoff before
// the tick tries it again, while a bead with no failure record is dispatched as
// usual (gt-811f6). The failed sling left the bead open and unassigned, so
// without the check every tick re-slings it into the same failure.
func TestSpecDispatchStartupBackoff(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := dispatch.RecordStartupFailure(townRoot, "gt-resting", "startup blocked: trust dialog"); err != nil {
		t.Fatal(err)
	}
	f := newFakeSpecTown(
		cleanSpec("gt-resting", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-fresh", 1, "2026-09-29T10:00:00Z"),
	)
	env := f.env()
	env.PerTick = 2
	env.StartupBackoff = func(id string) string { return dispatch.StartupBackoff(townRoot, id) }
	r := runSpecDispatchCycle(env)

	if len(f.slung) != 1 || f.slung[0] != "gt-fresh" {
		t.Fatalf("slung %v, want only gt-fresh: a resting bead waits out its backoff", f.slung)
	}
	want := "gt-resting: " + specdispatch.SkipStartupBackoff
	if len(r.Skipped) != 1 || !strings.HasPrefix(r.Skipped[0].Line, want) || !strings.Contains(r.Skipped[0].Line, "trust dialog") {
		t.Fatalf("skipped = %+v, want one line starting %q naming the failure", r.Skipped, want)
	}
}

// A bead resting out its backoff leaves the tick before it takes a seat, so the
// seat it would have used is still free: a bead pinned to the same agent and one
// pinned to another both dispatch on their own seat (gt-811f6).
func TestSpecDispatchStartupBackoffLeavesTheSeatFree(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := dispatch.RecordStartupFailure(townRoot, "gt-resting", "startup blocked: trust dialog"); err != nil {
		t.Fatal(err)
	}
	pinned := func(id, agent string) specdispatch.Spec {
		s := cleanSpec(id, 1, "2026-09-29T10:00:00Z")
		s.Labels = []string{"agent:" + agent}
		return s
	}
	f := newFakeSpecTown(
		pinned("gt-resting", "deepseek-flash"),
		pinned("gt-same", "deepseek-flash"),
		pinned("gt-other", "claude-sonnet"),
	)
	env := f.env()
	env.PerTick = 3
	env.StartupBackoff = func(id string) string { return dispatch.StartupBackoff(townRoot, id) }
	r := runSpecDispatchCycle(env)

	seats := map[string]string{}
	for i, id := range f.slung {
		seats[id] = f.slingSeats[i].Agent
	}
	if len(f.slung) != 2 || seats["gt-same"] != "deepseek-flash" || seats["gt-other"] != "claude-sonnet" {
		t.Fatalf("slung %v onto %v, want gt-same on deepseek-flash and gt-other on claude-sonnet", f.slung, f.slingSeats)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Bead != "gt-resting" {
		t.Fatalf("skipped = %+v, want only gt-resting", r.Skipped)
	}
}

func TestSpecDispatchRetriesDoltContention(t *testing.T) {
	t.Parallel()
	contention := errors.New("formula failed: bd mol bond: Error 1213 (40001): serialization failure")

	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{contention, contention}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.sleeps != 2 || !strings.Contains(r.Dispatched[0].Line, "after 3 attempts") {
		t.Fatalf("slung %v sleeps %d report %+v", f.slung, f.sleeps, r.Dispatched)
	}
}

// Contention that outlasts every retry is a deferral, not a failure: the next
// tick repeats the dispatch, since the bead is still unassigned. The label the
// failure path adds excludes the bead until an operator clears it,
// which is wrong for a bead the contention itself cleared (gt-9x16z).
func TestSpecDispatchContentionThatOutlastsItsRetriesIsADeferral(t *testing.T) {
	t.Parallel()
	contention := errors.New("formula failed: bd mol bond: Error 1213 (40001): serialization failure")
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	for i := 0; i < specdispatch.RetryAttempts; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contention)
	}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Failed) != 0 || len(r.Deferred) != 1 {
		t.Fatalf("slung %v failed %+v deferred %+v, want one deferral and no failure", f.slung, r.Failed, r.Deferred)
	}
	if line := r.Deferred[0].Line; !strings.Contains(line, "Dolt contention") || !strings.Contains(line, "after 4 attempts") {
		t.Errorf("deferral line = %q", line)
	}
	if n := len(f.notes["gt-a"]); n != 0 {
		t.Errorf("annotated %d times, want none: the bead is not held out of the queue", n)
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("labels = %v, want none: %s must not stick after contention clears", got, specdispatch.DispatchFailedLabel)
	}
	// The bead is still a candidate, so the next tick slings it once the
	// contention is gone.
	if r := runSpecDispatchCycle(f.env()); len(r.Dispatched) != 1 || len(f.slung) != 1 {
		t.Errorf("next tick: dispatched %+v, slung %v; want the bead slung", r.Dispatched, f.slung)
	}
}

// A sandbox image that lags the installed gt refuses the sling for the seat's
// sake, not the bead's: the dispatcher defers it, adds no failed label and no
// note, and the next tick slings it once the image is rebuilt. Any other sling
// failure still labels the bead.
func TestSpecDispatchImageDriftIsADeferralAndOtherFailuresAreNot(t *testing.T) {
	t.Parallel()
	drift := fmt.Errorf("starting polecat session: %w", fmt.Errorf("starting session: %w", session.ErrImageDrift))
	cases := []struct {
		name         string
		err          error
		wantFailed   int
		wantDeferred int
		wantLabel    bool
	}{
		{"image_drift_defers", drift, 0, 1, false},
		{"another_failure_labels", errors.New("starting polecat session: tmux: no server"), 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
			f.slingErrs["gt-a"] = []error{tc.err}
			r := runSpecDispatchCycle(f.env())
			if len(r.Failed) != tc.wantFailed || len(r.Deferred) != tc.wantDeferred || len(f.slung) != 0 {
				t.Fatalf("failed %+v deferred %+v slung %v; want %d failed and %d deferred", r.Failed, r.Deferred, f.slung, tc.wantFailed, tc.wantDeferred)
			}
			if got := len(f.labels["gt-a"]) > 0; got != tc.wantLabel {
				t.Errorf("labels = %v, want label added = %v", f.labels["gt-a"], tc.wantLabel)
			}
			if tc.wantLabel {
				return
			}
			if n := len(f.notes["gt-a"]); n != 0 {
				t.Errorf("annotated %d times, want none: the bead is not held out of the queue", n)
			}
			if line := r.Deferred[0].Line; !strings.Contains(line, "image lags the installed gt") || !strings.Contains(line, session.ErrImageDrift.Error()) {
				t.Errorf("deferral line = %q, want it to name the drift", line)
			}
			// Still a candidate: the next tick slings it once the image matches.
			if r := runSpecDispatchCycle(f.env()); len(r.Dispatched) != 1 || len(f.slung) != 1 {
				t.Errorf("next tick: dispatched %+v, slung %v; want the bead slung", r.Dispatched, f.slung)
			}
		})
	}
}

// A rig's cache volume that cannot be created defers the bead the way a drifted
// image does: no failed label, no note, and the next tick slings it once the
// volume exists (gt-td6pl).
func TestSpecDispatchCacheVolumeIsADeferralNotAFailure(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{fmt.Errorf("starting polecat session: %w", fmt.Errorf("creating sandbox: %w", session.ErrCacheVolume))}
	r := runSpecDispatchCycle(f.env())
	if len(r.Failed) != 0 || len(r.Deferred) != 1 || len(f.slung) != 0 {
		t.Fatalf("failed %+v deferred %+v slung %v; want 0 failed and 1 deferred", r.Failed, r.Deferred, f.slung)
	}
	if n := len(f.labels["gt-a"]); n != 0 {
		t.Errorf("labels = %v, want no spec-dispatch-failed label", f.labels["gt-a"])
	}
	if n := len(f.notes["gt-a"]); n != 0 {
		t.Errorf("annotated %d times, want none", n)
	}
	if line := r.Deferred[0].Line; !strings.Contains(line, "cache volume cannot be created") {
		t.Errorf("deferral line = %q, want it to name the volume", line)
	}
	if r := runSpecDispatchCycle(f.env()); len(r.Dispatched) != 1 || len(f.slung) != 1 {
		t.Errorf("next tick: dispatched %+v, slung %v; want the bead slung", r.Dispatched, f.slung)
	}
}

func TestSpecDispatchPoolRefusalIsASkip(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{&poolBackpressureError{Reason: "pool: seat full (2/2) -> no seat"}}
	r := runSpecDispatchCycle(f.env())
	if len(r.Skipped) != 1 || len(r.Failed) != 0 || len(f.notes["gt-a"]) != 0 || f.sleeps != 0 {
		t.Fatalf("pool refusal handled as failure: %+v notes %v", r, f.notes)
	}
}

// The tick charges a seat when it collects a candidate, so the next candidate
// of the tick sees it taken. A refused sling spawns no polecat and hands the
// seat back. A sling that fails in any other way may have hooked the bead or
// started a session before it returned, so its seat stays charged for the tick,
// and the next tick's snapshot recounts it (gt-9x16z, gt-pto2f).
func TestSpecRunPendingSlingsReleasesTheSeatASlingLeftEmpty(t *testing.T) {
	t.Parallel()
	overlap := fmt.Errorf("sling refused: %w", errSlingDuplicateContent)
	contention := errors.New("formula failed: bd mol bond: Error 1213 (40001): serialization failure")
	outlastedRetries := make([]error, specdispatch.RetryAttempts)
	for i := range outlastedRetries {
		outlastedRetries[i] = contention
	}
	cases := []struct {
		name          string
		slingErrs     []error
		spawnThenFail bool
		wantLive      int
	}{
		{name: "the overlap guard refuses the sling", slingErrs: []error{overlap}},
		{name: "the pool refuses the sling", slingErrs: []error{&poolBackpressureError{Reason: "pool: seat full (2/2) -> no seat"}}},
		{name: "contention outlasts every retry", slingErrs: outlastedRetries, wantLive: 1},
		{name: "the sling fails outright", slingErrs: []error{errors.New("sling failed: bd update: boom")}, wantLive: 1},
		{name: "the sandbox image lags the installed gt", slingErrs: []error{fmt.Errorf("starting polecat session: %w", session.ErrImageDrift)}, wantLive: 1},
		{name: "the sling fails after it spawned a polecat", spawnThenFail: true, wantLive: 1},
		{name: "the sling spawns a polecat", wantLive: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
			if len(tc.slingErrs) > 0 {
				f.slingErrs["gt-a"] = tc.slingErrs
			}
			env := f.env()
			if tc.spawnThenFail {
				env.Sling = func(specCandidate, string, specdispatch.SeatChoice) (string, error) {
					return "nux", errors.New("session failed: tmux: boom")
				}
			}
			budget := specdispatch.Budget{
				Seats: []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 3}},
				Now:   specTestNow,
			}
			// The charge the collection loop made for this candidate.
			budget.Bump("deepseek-flash", specTestNow)
			pending := []specPendingSling{{
				id:    "gt-a",
				c:     specCandidate{Spec: f.specs["gt-a"], Rig: "gastown"},
				seat:  specdispatch.SeatChoice{Agent: "deepseek-flash"},
				entry: specDispatchEntry{Bead: "gt-a", Rig: "gastown", Agent: "deepseek-flash"},
			}}
			var report specDispatchReport
			specRunPendingSlings(env, pending, &report, &budget)
			if got := budget.Seats[0].Live; got != tc.wantLive {
				t.Errorf("seat live = %d, want %d", got, tc.wantLive)
			}
			if got := budget.LiveTotal; got != tc.wantLive {
				t.Errorf("live total = %d, want %d", got, tc.wantLive)
			}
		})
	}
}

// A dispatch that fails for a reason it cannot clear itself leaves the queue
// under the spec-dispatch-failed label, and the tick says exactly that: the
// bead id, the reason, and the label that holds it out (gt-q6zoo).
func TestSpecDispatchFailureNamesTheExclusion(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{errors.New("rig gastown is down")}
	r := runSpecDispatchCycle(f.env())
	if len(r.Failed) != 1 {
		t.Fatalf("failed = %+v, want one entry", r.Failed)
	}
	for _, want := range []string{"gt-a", "rig gastown is down", specdispatch.DispatchFailedLabel, "excluded from the queue"} {
		if !strings.Contains(r.Failed[0].Line, want) {
			t.Errorf("failure line %q missing %q", r.Failed[0].Line, want)
		}
	}
	if got := f.labels["gt-a"]; len(got) != 1 || got[0] != specdispatch.DispatchFailedLabel {
		t.Fatalf("labels = %v, want %s once", got, specdispatch.DispatchFailedLabel)
	}
	notes := f.notes["gt-a"]
	if len(notes) != 1 || !strings.Contains(notes[0], specdispatch.DispatchFailedLabel) {
		t.Errorf("bead notes = %v, want one naming the label", notes)
	}

	// The label is what excludes it: the next tick reads the bead, counts it
	// and takes no candidate from it.
	r = runSpecDispatchCycle(f.env())
	if r.Candidates != 0 || r.LabeledFailed != 1 || len(r.Dispatched) != 0 {
		t.Errorf("next tick = %d candidate(s), %d labeled-failed, %d dispatched; want 0, 1, 0", r.Candidates, r.LabeledFailed, len(r.Dispatched))
	}
}

// A sling-stage deferral is the reason a chosen candidate did not go out, and
// the tick line caps its skip list, so a deferral in the tail read as "+N more"
// and neither the bead nor the reason survived in the log (gt-gi3f9). The
// report carries the sling-stage part of Skipped on its own so the daemon can
// log one line per deferral, while Skipped stays exactly as it was, so the tick
// line counts and truncates as before (gt-4wcqd).
func TestSpecDispatchDeferralIsReportedOutsideTheSkipList(t *testing.T) {
	t.Parallel()
	t.Run("a deferred sling is reported beside the unchanged skip", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
		f.slingErrs["gt-a"] = []error{fmt.Errorf("%s gt-a is held (label needs-human): the hold was written after this dispatch read the bead", dispatch.SlingRefusalMarker)}
		r := runSpecDispatchCycle(f.env())
		if len(r.Deferred) != 1 || r.Deferred[0].Bead != "gt-a" || !strings.Contains(r.Deferred[0].Line, "is held") {
			t.Fatalf("deferred = %+v, want gt-a's deferral with its reason", r.Deferred)
		}
		if len(r.Skipped) != 1 || r.Skipped[0].Line != r.Deferred[0].Line {
			t.Errorf("skipped = %+v, want the deferral still listed unchanged so the tick line reads as before", r.Skipped)
		}
	})

	t.Run("a failed sling is not a deferral", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
		// A failure the bead cannot clear itself is logged under failed, and no
		// deferred line repeats it (gt-4wcqd).
		f.slingErrs["gt-a"] = []error{errors.New("rig gastown is down")}
		r := runSpecDispatchCycle(f.env())
		if len(r.Failed) != 1 || len(r.Deferred) != 0 {
			t.Fatalf("failed = %+v deferred = %+v, want one failure and no deferral", r.Failed, r.Deferred)
		}
	})

	t.Run("a dry run reports neither", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
		f.slingErrs["gt-a"] = []error{fmt.Errorf("%s gt-a is held (label needs-human): the hold was written after this dispatch read the bead", dispatch.SlingRefusalMarker)}
		env := f.env()
		env.DryRun = true
		r := runSpecDispatchCycle(env)
		if len(r.Deferred) != 0 || len(r.Failed) != 0 || len(r.Dispatched) != 1 {
			t.Fatalf("dry run = %+v, want one would-sling and nothing deferred or failed", r)
		}
	})
}

// Clearing the label retries the bead. When the retry fails for another reason,
// the bead carries both notes, and the same reason failing again adds none
// (gt-zkqtn).
func TestSpecDispatchFailureNoteFollowsTheReason(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	retry := func(reason string) {
		s := f.specs["gt-a"]
		s.Labels = nil
		f.specs["gt-a"] = s
		f.slingErrs["gt-a"] = []error{errors.New(reason)}
		if r := runSpecDispatchCycle(f.env()); len(r.Failed) != 1 {
			t.Fatalf("%s: failed = %+v, want one entry", reason, r.Failed)
		}
	}
	retry("rig gastown is down")
	retry("rig gastown is down")
	retry("no polecat slot")

	notes := f.notes["gt-a"]
	if len(notes) != 2 || !strings.Contains(notes[0], "rig gastown is down") || !strings.Contains(notes[1], "no polecat slot") {
		t.Errorf("bead notes = %q, want one per distinct reason, in order", notes)
	}
}

// A bead that already carries the note an earlier dispatcher wrote keeps it as
// the one note for that reason, and a retry that fails differently adds one
// (gt-0vz9k).
func TestSpecDispatchFailureNoteOnABeadWithTheOldNote(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	old := fmt.Sprintf("%sgt-a: dispatch failed, left unassigned: rig gastown is down; excluded from the queue until label %s is cleared", specDispatchNotePrefix, specdispatch.DispatchFailedLabel)
	f.notes["gt-a"] = []string{old}
	retry := func(reason string) {
		s := f.specs["gt-a"]
		s.Labels = nil
		f.specs["gt-a"] = s
		f.slingErrs["gt-a"] = []error{errors.New(reason)}
		if r := runSpecDispatchCycle(f.env()); len(r.Failed) != 1 {
			t.Fatalf("%s: failed = %+v, want one entry", reason, r.Failed)
		}
	}
	retry("rig gastown is down")
	if notes := f.notes["gt-a"]; len(notes) != 1 || notes[0] != old {
		t.Fatalf("notes after the same reason = %q, want only the old note", notes)
	}
	retry("no polecat slot")
	if notes := f.notes["gt-a"]; len(notes) != 2 || notes[0] != old || !strings.Contains(notes[1], "no polecat slot") {
		t.Errorf("notes after a new reason = %q, want the old note then one for the new reason", notes)
	}
}

func TestSpecDispatchDryRunTouchesNothing(t *testing.T) {
	t.Parallel()
	bad := cleanSpec("gt-bad", 1, "2026-09-29T10:00:00Z")
	bad.Type = "epic" // not a work bead, so not a candidate
	good := cleanSpec("gt-good", 1, "2026-09-29T11:00:00Z")
	f := newFakeSpecTown(good)
	f.specs["gt-bad"] = bad // not eligible; exercised via the Show path below
	env := f.env()
	env.DryRun = true
	r := runSpecDispatchCycle(env)
	if len(f.slung) != 0 || len(f.notes) != 0 || len(f.labels) != 0 {
		t.Fatalf("dry run had side effects: slung %v notes %v labels %v", f.slung, f.notes, f.labels)
	}
	if len(r.Dispatched) != 1 || !strings.Contains(r.Dispatched[0].Line, "would sling") {
		t.Fatalf("dry run report = %+v", r)
	}
}

// TestSpecDispatchHoldsAtTheTownTotal is the town cap on top of the seats'
// own (gt-bcpj5): the pool seat has room and the other seat is full, and the
// town's total is what holds the bead — the state two agents each inside their
// own cap produced, 5 live polecats where 3 was wanted. The picture the tick
// reports names the total, so the operator reads the cap that held it.
func TestSpecDispatchHoldsAtTheTownTotal(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.maxTotal = 3
	f.roster = specRoster{Live: map[string]int{"deepseek-flash": 1, "claude-sonnet": 2}}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want none: the town is at its total", f.slung)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Line != "gt-a: no seat: over the town total: 3/3 live polecats across every seat" {
		t.Fatalf("skipped = %+v, want the town total named", r.Skipped)
	}
	if r.Roster != "deepseek-flash 1/2, claude-sonnet 2/2, total 3/3" {
		t.Errorf("roster = %q, want the total in the picture", r.Roster)
	}

	// One polecat leaves, and the seat that has room spends it: the total is
	// the only thing that was holding the bead.
	f.roster = specRoster{Live: map[string]int{"deepseek-flash": 1, "claude-sonnet": 1}}
	r = runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || len(r.Skipped) != 0 {
		t.Fatalf("slung %v skipped %+v, want the dispatch a town under its total takes", f.slung, r.Skipped)
	}
}

// specSeatTown is town settings whose pool holds seats. Each seat's agent is
// defined in agents, so the loader's known-agent check (the one a tick reads
// through) passes without a settings file on disk.
func specSeatTown(seats map[string]config.SeatConfig) *config.TownSettings {
	ts := config.NewTownSettings()
	ts.Agents = map[string]*config.RuntimeConfig{}
	for agent := range seats {
		ts.Agents[agent] = &config.RuntimeConfig{Command: "claude"}
	}
	ts.PolecatPool = &config.PolecatPool{Seats: seats}
	return ts
}

// The town's total reaches the tick's budget from polecat_pool.max_total
// (gt-bcpj5). An absent key and a written 0 are the same town — one with no
// total cap — which is why every reader tests the value above zero.
func TestSpecBudgetCarriesTheTownTotal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		maxTotal *int
		want     int
	}{
		{"absent", nil, 0},
		{"0", intPtr(0), 0},
		{"4", intPtr(4), 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := specSeatTown(map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}})
			ts.PolecatPool.MaxTotal = tc.maxTotal
			b, err := specBudgetFor("", ts, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if b.MaxTotal != tc.want {
				t.Errorf("budget MaxTotal = %d, want %d", b.MaxTotal, tc.want)
			}
		})
	}
}

// The tick's budget is one seat per entry of the seat map at its own cap, in
// the loader's order, with the pool's spawn gap. A cap of 0 is a closed seat:
// it is listed at 0 and takes nothing, where the retired max_seats read 0 as a
// default of 2 (gt-mukgk).
func TestSpecBudgetForSeats(t *testing.T) {
	t.Parallel()
	ts := specSeatTown(map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}})
	ts.PolecatPool.MinSpawnGap = "4m"

	b, err := specBudgetFor("", ts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Picture(); got != "deepseek-flash 0/3" || b.MinSpawnGap != 4*time.Minute {
		t.Fatalf("one seat = %q gap %v", got, b.MinSpawnGap)
	}

	ts = specSeatTown(map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}, "claude-sonnet": {Cap: 1}, "claude-haiku": {Cap: 0}})
	b, err = specBudgetFor("", ts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Picture(); got != "claude-haiku 0/0, claude-sonnet 0/1, deepseek-flash 0/3" {
		t.Fatalf("three seats = %q, want every seat by name with its own cap", got)
	}
	if b.Seats[0].FreeAt(b.Now) {
		t.Errorf("a seat with a cap of 0 takes a spawn: %+v", b.Seats[0])
	}
}

// A pool with no seat has none for the dispatcher to fill: the pool's own
// admission point says the same (choosePoolAgent). A seat map the loader
// rejects stops the tick's budget rather than running on part of the list.
func TestSpecBudgetWithoutAPoolSeat(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	for name, pool := range map[string]*config.PolecatPool{
		"nil pool":       nil,
		"empty pool":     {},
		"max_total only": {MaxTotal: intPtr(3)},
	} {
		ts.PolecatPool = pool
		b, err := specBudgetFor("", ts, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := b.Picture(); got != "no seats" {
			t.Errorf("%s = %q, want no seats", name, got)
		}
	}

	if b, err := specBudgetFor("", nil, time.Now()); err != nil || b.Picture() != "no seats" {
		t.Errorf("nil settings = %q, %v, want no seats", b.Picture(), err)
	}

	ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{"no-such-agent": {Cap: 1}}}
	if _, err := specBudgetFor("", ts, time.Now()); err == nil || !strings.Contains(err.Error(), "polecat_pool.seats.no-such-agent") {
		t.Errorf("an unknown agent = %v, want the loader's message naming the seat", err)
	}
}

func intPtr(n int) *int { return &n }

func TestSpecRosterCountsByAgent(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = seatPool("deepseek-flash", 3)
	now := time.Now()
	r := specRosterFrom([]poolSession{
		{name: "a", agent: "claude-sonnet", created: now.Add(-time.Hour)},
		{name: "b", agent: "deepseek-flash", created: now.Add(-2 * time.Hour)},
		{name: "c", agent: "", created: now.Add(-time.Minute)}, // the pool's only seat
		{name: "pool-claim/x", agent: "deepseek-flash", created: now.Add(-30 * time.Second)},
	}, ts)
	if r.Live["claude-sonnet"] != 1 || r.Live["deepseek-flash"] != 3 || !r.Newest.Equal(now.Add(-30*time.Second)) {
		t.Fatalf("roster = %+v", r)
	}
}

// TestSpecRosterCountsDeadHookedSeats pins gt-tldj4's roster half: a seat whose
// session is gone while its bead is still hooked counts in Live and is broken
// out in DeadHooked, so the tick line can say why a seat reads full with fewer
// live sessions. Its zero spawn time must not arm the stagger.
func TestSpecRosterCountsDeadHookedSeats(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = seatPool("deepseek-flash", 3)
	now := time.Now()
	r := specRosterFrom([]poolSession{
		{name: "gt-live", agent: "deepseek-flash", created: now.Add(-time.Hour)},
		{name: "dead/gastown/ember", agent: "deepseek-flash", deadHooked: true},
	}, ts)
	if r.Live["deepseek-flash"] != 2 {
		t.Fatalf("a dead-hooked seat counts toward the cap: %+v", r)
	}
	if r.DeadHooked["deepseek-flash"] != 1 {
		t.Fatalf("the dead-hooked seat must be broken out: %+v", r)
	}
	if !r.Newest.Equal(now.Add(-time.Hour)) {
		t.Fatalf("a dead seat has no spawn time and must not arm the stagger: Newest = %v", r.Newest)
	}
}

func TestSpecLintCommandExitCodes(t *testing.T) {
	t.Parallel()
	tmpl := filepath.Join(t.TempDir(), "spec-template.md")
	if err := os.WriteFile(tmpl, []byte("--description=\"## Goal\n## Constraints\n## Out of scope\n## Gate\n## Size\n\""), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, json, want string
		code             int
	}{
		// A task with no spec label lints clean: shape is the whole gate.
		{"clean", `[{"id":"gt-ok","issue_type":"task","status":"open","description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`, "gt-ok: spec lint ok", 0},
		{"refused", `[{"id":"gt-no","issue_type":"bug","status":"open","description":"## Goal\nx"}]`, "gt-no: spec lint refused: ## Constraints: section missing", 1},
		{"planning", `[{"id":"gt-pl","issue_type":"task","status":"open","labels":["needs-planning"],"description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`, "gt-pl: spec needs planning: label needs-planning", 2},
		{"not a work bead", `[{"id":"gt-epic","issue_type":"epic","status":"open","description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`, "gt-epic: spec lint refused: not a work bead: type epic", 1},
		{"unreadable bead", "", "gt-gone: spec lint refused: bead: bead gt-gone not found", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beadID := strings.SplitN(tc.want, ":", 2)[0]
			var spec specdispatch.Spec
			var showErr error
			if tc.json == "" {
				showErr = fmt.Errorf("bead %s not found", beadID)
			} else {
				var issues []beads.Issue
				if err := json.Unmarshal([]byte(tc.json), &issues); err != nil {
					t.Fatal(err)
				}
				spec = specFromIssue(&issues[0])
			}
			var out, errOut bytes.Buffer
			err := specLint(&out, &errOut, beadID, spec, showErr, tmpl, false)
			code, _ := IsSilentExit(err)
			if err != nil && code == 0 {
				t.Fatalf("unexpected error %v", err)
			}
			if code != tc.code {
				t.Errorf("exit code = %d, want %d", code, tc.code)
			}
			if got := strings.TrimSpace(out.String()); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
			// A non-zero exit carries the skeleton on stderr; a clean bead
			// prints nothing extra (gt-ngtev).
			stderr := errOut.String()
			if code == 0 {
				if stderr != "" {
					t.Errorf("clean bead wrote to stderr: %q", stderr)
				}
				return
			}
			for _, want := range append([]string{"Acceptance"}, specdispatch.DefaultSections...) {
				if !strings.Contains(stderr, "## "+want+"\n") {
					t.Errorf("stderr is missing section %q:\n%s", want, stderr)
				}
			}
			if !strings.Contains(stderr, "run gt spec lint "+beadID+" again.") {
				t.Errorf("stderr is missing the re-lint command:\n%s", stderr)
			}
		})
	}
}

// gt spec lint --json prints {id, ok, needs_planning, refusals[]} so a shell
// caller can act on every refusal, with the exit codes unchanged.
func TestSpecLintJSONReport(t *testing.T) {
	t.Parallel()
	tmpl := filepath.Join(t.TempDir(), "spec-template.md")
	if err := os.WriteFile(tmpl, []byte("--description=\"## Goal\n## Constraints\n## Out of scope\n## Gate\n## Size\n\""), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		json         string
		wantOK       bool
		wantPlanning bool
		wantFields   string
		wantExit     int
	}{
		{
			name:       "clean",
			json:       `[{"id":"gt-ok","issue_type":"task","status":"open","description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`,
			wantOK:     true,
			wantFields: "",
			wantExit:   0,
		},
		{
			name:       "refused lists every failure",
			json:       `[{"id":"gt-nope","issue_type":"task","status":"open","description":"## Goal\nx"}]`,
			wantOK:     false,
			wantFields: "## Constraints|## Out of scope|## Gate|## Size|acceptance",
			wantExit:   1,
		},
		{
			name:         "planning",
			json:         `[{"id":"gt-pl","issue_type":"task","status":"open","labels":["needs-planning"],"description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`,
			wantOK:       false,
			wantPlanning: true,
			wantFields:   "",
			wantExit:     2,
		},
		{
			name:       "not a work bead",
			json:       `[{"id":"gt-epic","issue_type":"epic","status":"open","description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`,
			wantOK:     false,
			wantFields: "not a work bead",
			wantExit:   1,
		},
		{
			// A live wisp (bd show gt-wisp-pbug9, 2026-10-02): type molecule,
			// ephemeral true, a clean-looking description. Must still refuse.
			name:       "wisp",
			json:       `[{"id":"gt-wisp-x","issue_type":"molecule","ephemeral":true,"status":"open","description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`,
			wantOK:     false,
			wantFields: "not a work bead",
			wantExit:   1,
		},
		{
			name:       "unreadable bead",
			json:       "",
			wantOK:     false,
			wantFields: "bead",
			wantExit:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beadID := "gt-gone"
			spec := specdispatch.Spec{}
			var showErr error
			if tc.json != "" {
				var issues []beads.Issue
				if err := json.Unmarshal([]byte(tc.json), &issues); err != nil {
					t.Fatal(err)
				}
				beadID, spec = issues[0].ID, specFromIssue(&issues[0])
			} else {
				showErr = fmt.Errorf("bead %s not found", beadID)
			}
			var out, errOut bytes.Buffer
			err := specLint(&out, &errOut, beadID, spec, showErr, tmpl, true)
			code, _ := IsSilentExit(err)
			if code != tc.wantExit {
				t.Fatalf("exit code = %d, want %d (%v)", code, tc.wantExit, err)
			}
			if errOut.Len() != 0 {
				t.Errorf("--json wrote the skeleton to stderr: %q", errOut.String())
			}
			var got specLintReport
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out.String())
			}
			if got.ID != beadID || got.OK != tc.wantOK || got.NeedsPlanning != tc.wantPlanning {
				t.Errorf("report = %+v, want id %s ok %v planning %v", got, beadID, tc.wantOK, tc.wantPlanning)
			}
			var fields []string
			for _, r := range got.Refusals {
				if r.Reason == "" {
					t.Errorf("refusal %+v has no reason", r)
				}
				fields = append(fields, r.Field)
			}
			if strings.Join(fields, "|") != tc.wantFields {
				t.Errorf("refusal fields = %q, want %q", strings.Join(fields, "|"), tc.wantFields)
			}
			if got.Refusals == nil {
				t.Error("refusals must serialize as [], not null")
			}
		})
	}
}

// specTown is a temp town with one operational rig whose .beads looks like a
// database, so specCandidates walks it. The board itself is served by the
// specReadyBoard seam.
func specTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	// mayor/town.json makes the town load as a town; without it every rig
	// reads as parked (townconfig.IsParked fails closed).
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{"gastown": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// specCandidates is the dispatcher's intake, so it is the place the
// acceptance case lives: a ready, unassigned, open task/bug/feature bead
// without the retired spec label is a candidate; the skips
// (gt:ready-to-land, a READY TO LAND block with no label yet, needs-human,
// deferred, assigned), the non-work types and the operator's ceiling are not
// (gt-4k3fj.8.8 acceptance 1 and 2). The board comes from a fake store, so this
// pins the filter rather than bd's own.
func TestSpecCandidatesFromAFakeStore(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	fakeBoard := func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "gt-task", Type: "task", Status: "open", Priority: 2},
			{ID: "gt-bug", Type: "bug", Status: "open", Priority: 1},
			{ID: "gt-feature", Type: "feature", Status: "open", Priority: 3},
			{ID: "gt-chore", Type: "chore", Status: "open", Priority: 1},
			{ID: "gt-assigned", Type: "task", Status: "open", Priority: 1, Assignee: "gastown/polecats/ruby"},
			{ID: "gt-landing", Type: "task", Status: "open", Priority: 1, Labels: []string{"gt:ready-to-land"}},
			// The label-less half of the same window: the READY TO LAND block
			// is written first, so an answer that lags the label still carries
			// the block and the bead stays off the board (gt-kr5xv).
			{ID: "gt-mid", Type: "task", Status: "open", Priority: 1, Notes: readyBlock("sloan/x", "abc1234", "main")},
			{ID: "gt-human", Type: "task", Status: "open", Priority: 1, Labels: []string{"needs-human"}},
			{ID: "gt-deferred", Type: "task", Status: "deferred", Priority: 1},
			{ID: "gt-epic", Type: "epic", Status: "open", Priority: 1},
			{ID: "gt-p4", Type: "task", Status: "open", Priority: 4},
			{ID: "gt-stuck", Type: "task", Status: "open", Priority: 1, Labels: []string{specdispatch.DispatchFailedLabel}},
		}, nil
	}

	idList := func(cs []specCandidate) string {
		var out []string
		for _, c := range cs {
			if c.Rig != "gastown" {
				t.Errorf("candidate %s carries rig %q, want gastown", c.Spec.ID, c.Rig)
			}
			out = append(out, c.Spec.ID)
		}
		return strings.Join(out, " ")
	}

	got := specCandidates(townRoot, 2, fakeBoard)
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
	if ids := idList(got.Candidates); ids != "gt-bug gt-task" {
		t.Errorf("candidates at the default ceiling = %q, want the P1 bug then the P2 task", ids)
	}
	// gt-stuck is ready, but spec-dispatch-failed holds it out of the queue:
	// the read counts it instead of taking it, so the tick can surface the
	// count (gt-q6zoo).
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed = %d, want 1", got.LabeledFailed)
	}

	// The ceiling is the operator's (polecat_pool.max_priority): at P4 the
	// feature and the P4 task are the dispatcher's work too.
	got = specCandidates(townRoot, 4, fakeBoard)
	if ids := idList(got.Candidates); ids != "gt-bug gt-task gt-feature gt-p4" {
		t.Errorf("candidates at a P4 ceiling = %q", ids)
	}
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed at a P4 ceiling = %d, want 1", got.LabeledFailed)
	}
}

// A bead the spec-dispatch-failed label holds out because its sling was killed
// at its bound is slung again while its retries remain: the read clears the
// label, the bead is a candidate of this tick, and the tick says so. A labeled
// bead the retry answers no for is still counted, not taken (gt-vaivp).
func TestSpecCandidatesRetriesASlingKilledAtItsBound(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	board := func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "gt-retry", Type: "task", Status: "open", Priority: 2, Labels: []string{specdispatch.DispatchFailedLabel}},
			{ID: "gt-other", Type: "task", Status: "open", Priority: 2, Labels: []string{specdispatch.DispatchFailedLabel}},
		}, nil
	}
	retry := func(id string) (bool, error) { return id == "gt-retry", nil }

	got := specCandidatesRetrying(townRoot, 2, board, func(string, string) (bool, string) { return false, "" }, retry)
	if len(got.Candidates) != 1 || got.Candidates[0].Spec.ID != "gt-retry" {
		t.Fatalf("candidates = %+v, want the retried bead alone", got.Candidates)
	}
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed = %d, want the bead the retry did not clear", got.LabeledFailed)
	}
	if len(got.Retried) != 1 || !strings.Contains(got.Retried[0], "gt-retry") {
		t.Errorf("Retried = %v, want the cleared bead named", got.Retried)
	}
}

// A clear that fails is reported and the bead stays held: a label that could not
// be read off is still holding the bead, so the tick must not sling it blind
// (gt-vaivp).
func TestSpecCandidatesReportsAFailedRetry(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	board := func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-stuck", Type: "task", Status: "open", Priority: 2, Labels: []string{specdispatch.DispatchFailedLabel}}}, nil
	}
	retry := func(string) (bool, error) { return false, errors.New("dolt: connection refused") }

	got := specCandidatesRetrying(townRoot, 2, board, func(string, string) (bool, string) { return false, "" }, retry)
	if len(got.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none for a bead whose label could not be cleared", got.Candidates)
	}
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed = %d, want the bead still counted as held", got.LabeledFailed)
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "dolt: connection refused") {
		t.Errorf("errors = %v, want the failed clear named", got.Errors)
	}
}

// specRetryTimedOutSlingFrom clears the label of a bead whose notes record a
// sling killed at its bound and a retry left, and leaves every other failed
// bead alone.
func TestSpecRetryTimedOutSlingFrom(t *testing.T) {
	t.Parallel()
	timeoutNote := specdispatch.TimeoutNoteMarker + " sling killed at its 5m0s bound (attempt 1 of 3)"
	cases := []struct {
		name      string
		labels    []string
		notes     string
		wantClear bool
		wantHeld  bool
	}{
		{"a kill with retries left", []string{specdispatch.DispatchFailedLabel}, timeoutNote, true, false},
		{"a failure of its own", []string{specdispatch.DispatchFailedLabel}, specDispatchNotePrefix + "gt-x: dispatch failed, left unassigned: boom", false, true},
		{"the retries spent", []string{specdispatch.DispatchFailedLabel},
			strings.Repeat(specdispatch.TimeoutNoteMarker+" an earlier kill\n", specdispatch.TimeoutRetries), false, true},
		{"no label to clear", nil, timeoutNote, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			db.Seed(beads.Issue{ID: "gt-x", Status: "open", Labels: tc.labels, Notes: tc.notes})

			cleared, err := specRetryTimedOutSlingFrom(db, "gt-x")
			if err != nil {
				t.Fatalf("specRetryTimedOutSlingFrom: %v", err)
			}
			if cleared != tc.wantClear {
				t.Errorf("cleared = %v, want %v", cleared, tc.wantClear)
			}
			issue, err := db.Show("gt-x")
			if err != nil {
				t.Fatal(err)
			}
			if held := slices.Contains(issue.Labels, specdispatch.DispatchFailedLabel); held != tc.wantHeld {
				t.Errorf("label held = %v, want %v", held, tc.wantHeld)
			}
		})
	}
}

// A rig whose park state cannot be read is held out of the candidate set, and
// the read says why. An unverified rig is reported as an error line, the way a
// failed board read is, so the tick log shows it was held for lack of a
// verdict. A parked rig stays quiet: parking is the operator's choice, not a
// fault to surface (gt-6d06g).
func TestSpecCandidatesReportsAnUnverifiedRig(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	board := func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-open", Type: "bug", Status: "open", Priority: 2}}, nil
	}

	cases := []struct {
		name       string
		reason     string
		wantErrors int
	}{
		{"unverified", sling.RigStatusUnverified, 1},
		{"parked", "parked", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			blocked := func(string, string) (bool, string) { return true, tc.reason }

			got := specCandidatesWith(townRoot, 2, board, blocked)
			if len(got.Candidates) != 0 {
				t.Errorf("a blocked rig yielded candidates %+v", got.Candidates)
			}
			if len(got.Errors) != tc.wantErrors {
				t.Errorf("errors = %v, want %d line(s) for a %s rig", got.Errors, tc.wantErrors, tc.reason)
			}
		})
	}
}

// A ready, shaped bead a person labeled crew-only is skipped,
// and the read says so: the skip line names the label, so the tick log and the
// dry run show an operator why the bead never took a seat instead of dropping
// it unseen. Its unlabelled twin, same type and priority, still dispatches
// (gt-k1g3e).
func TestSpecCandidatesSkipsCrewOnlyLabel(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	fakeBoard := func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "gt-crew", Type: "bug", Status: "open", Priority: 2, Labels: []string{specdispatch.CrewOnlyLabel}},
			{ID: "gt-open", Type: "bug", Status: "open", Priority: 2},
		}, nil
	}

	got := specCandidates(townRoot, 2, fakeBoard)
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
	var ids []string
	for _, c := range got.Candidates {
		ids = append(ids, c.Spec.ID)
	}
	if joined := strings.Join(ids, " "); joined != "gt-open" {
		t.Errorf("candidates = %q, want the unlabelled twin alone", joined)
	}
	if len(got.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the one crew-only refusal", got.Skipped)
	}
	e := got.Skipped[0]
	if e.Bead != "gt-crew" || e.Rig != "gastown" {
		t.Errorf("skip entry = %+v, want gt-crew in gastown", e)
	}
	if !strings.Contains(e.Line, specdispatch.CrewOnlyLabel) {
		t.Errorf("skip line %q does not name %s", e.Line, specdispatch.CrewOnlyLabel)
	}
}

// A ready, shaped human-only bead is skipped the same way, and so is one that
// also wears rework: only a person can produce the branch of such a bead, so a
// seat on it is a session that cannot land its work whoever holds it (gt-me800).
// Both refusals name the label, and the unlabelled twin still dispatches.
func TestSpecCandidatesSkipsHumanOnlyLabel(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	fakeBoard := func(string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "gt-human", Type: "bug", Status: "open", Priority: 2, Labels: []string{dispatch.HumanOnlyLabel}},
			{ID: "gt-human-rework", Type: "bug", Status: "open", Priority: 2,
				Labels: []string{dispatch.HumanOnlyLabel, land.LabelRework}},
			{ID: "gt-open", Type: "bug", Status: "open", Priority: 2},
		}, nil
	}

	got := specCandidates(townRoot, 2, fakeBoard)
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
	var ids []string
	for _, c := range got.Candidates {
		ids = append(ids, c.Spec.ID)
	}
	if joined := strings.Join(ids, " "); joined != "gt-open" {
		t.Errorf("candidates = %q, want the unlabelled twin alone", joined)
	}
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want both human-only refusals", got.Skipped)
	}
	for _, e := range got.Skipped {
		if e.Rig != "gastown" || !strings.Contains(e.Line, dispatch.HumanOnlyLabel) {
			t.Errorf("skip entry = %+v, want a gastown line naming %s", e, dispatch.HumanOnlyLabel)
		}
	}
}

// A board-carried refusal reaches the tick's skip lines, which is where the
// operator reads the dispatcher's decision: the daemon logs them from the
// report's skipped list and prints them under --dry-run
// (internal/daemon.SpecDispatchTickLine). A crew-only bead is never slung
// under either (gt-k1g3e).
func TestSpecDispatchReportsCrewOnlySkip(t *testing.T) {
	t.Parallel()
	crew := cleanSpec("gt-crew", 1, "2026-09-29T10:00:00Z")
	crew.Labels = []string{specdispatch.CrewOnlyLabel}
	for _, dryRun := range []bool{false, true} {
		f := newFakeSpecTown(crew, cleanSpec("gt-open", 2, "2026-09-29T11:00:00Z"))
		env := f.env()
		env.DryRun = dryRun
		r := runSpecDispatchCycle(env)
		var found bool
		for _, e := range r.Skipped {
			if e.Bead == "gt-crew" && strings.Contains(e.Line, specdispatch.CrewOnlyLabel) {
				found = true
			}
		}
		if !found {
			t.Errorf("dry-run=%v skipped = %+v, want a line naming %s", dryRun, r.Skipped, specdispatch.CrewOnlyLabel)
		}
		for _, id := range f.slung {
			if id == "gt-crew" {
				t.Fatalf("dry-run=%v slung the crew-only bead", dryRun)
			}
		}
		// The tick's own printout is the operator's view: the same report under
		// both flags has to carry the line.
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		printSpecDispatchReport(cmd, r)
		if !strings.Contains(out.String(), specdispatch.CrewOnlyLabel) {
			t.Errorf("dry-run=%v report output %q does not name %s", dryRun, out.String(), specdispatch.CrewOnlyLabel)
		}
	}
}

// capturedSpecBoard is the board the dispatcher reads, as the dispatcher reads
// it: the bytes of testdata/spec_ready_labels.json — a `bd ready --json`
// response captured from the live gastown rig on 2026-10-02 with the query
// specReadyOptions names — through beads.DecodeReadyBoard, the reader
// ReadyFiltered uses.
//
// The fakes elsewhere in this file hand the board a slice of beads.Issue
// values with Labels filled in, so they cannot catch a read path that never
// carries labels at all. This one starts from bd's own JSON through the
// production decoder, which is where om's critical finding lived (gt-q6zoo
// attempt 1); keeping the decoder in the path is what makes the next shape
// change in it visible here (gt-p85pw.7).
func capturedSpecBoard(t *testing.T) specBoard {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "spec_ready_labels.json"))
	if err != nil {
		t.Fatalf("captured board fixture: %v", err)
	}
	return func(string) ([]*beads.Issue, error) {
		issues, _, err := beads.DecodeReadyBoard(data)
		return issues, err
	}
}

// The count of beads the spec-dispatch-failed label holds comes from the ready
// board the dispatcher already reads, not a second query, so the count is real
// only if that board carries labels. `bd ready --json` does: it hydrates a
// "labels" array on every issue that has one, and the captured board has one
// bead wearing a real unrelated label and one wearing the dispatcher's own. A
// bd whose ready JSON dropped labels would fail this test (om's critical
// finding; the "bd ready --json doesn't include labels" note in ready.go
// describes the store-backed readers, which never see this JSON).
func TestCapturedSpecBoardCarriesLabels(t *testing.T) {
	t.Parallel()
	issues, err := capturedSpecBoard(t)("")
	if err != nil {
		t.Fatalf("decode the captured board: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("the captured board decoded to no issues")
	}

	var labeled, unrelated, unlabeled int
	for _, issue := range issues {
		s := specFromIssue(issue)
		switch {
		case s.HasLabel(specdispatch.DispatchFailedLabel):
			labeled++
			if issue.ID != "gt-8xk9k" {
				t.Errorf("bead %s carries %s; the fixture labels gt-8xk9k", issue.ID, specdispatch.DispatchFailedLabel)
			}
		case len(s.Labels) > 0:
			// gt-wlm2a wears "gt:task" in the capture: the array surviving
			// the decode, rather than this one label matching by accident.
			unrelated++
			if !s.HasLabel("gt:task") {
				t.Errorf("bead %s lost its captured labels: got %v", issue.ID, s.Labels)
			}
		default:
			unlabeled++
		}
	}
	if labeled != 1 || unrelated != 1 || unlabeled != 1 {
		t.Errorf("captured board decoded to %d labeled, %d unrelated-label, %d unlabeled beads; want 1 of each", labeled, unrelated, unlabeled)
	}
}

// specCandidates counts the beads the label holds in the same pass that builds
// the candidate list, from the same board. Driving it with the captured board
// is what would have caught a read path that dropped labels: the count would
// read 0 and the labeled bead would be admitted as a candidate (om, gt-q6zoo
// attempt 1).
func TestSpecCandidatesCountsTheLabeledFailedFromACapturedBoard(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)

	got := specCandidates(townRoot, 3, capturedSpecBoard(t))
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed = %d, want 1: the captured board carries gt-8xk9k's labels", got.LabeledFailed)
	}
	for _, c := range got.Candidates {
		if c.Spec.ID == "gt-8xk9k" {
			t.Errorf("gt-8xk9k is a candidate; %s must hold it out of the queue", specdispatch.DispatchFailedLabel)
		}
	}
	// The three captured beads are P3, unassigned and open: the two without
	// the label are candidates at a P3 ceiling and the labeled one is not.
	if len(got.Candidates) != 2 {
		t.Errorf("candidates = %d, want the two unlabeled P3 beads", len(got.Candidates))
	}
}

// The crew-only skip is read off the same board as everything else, so it
// depends on bd's ready JSON carrying labels — the gap om found in the
// LabeledFailed count (gt-q6zoo attempt 1). Here the board is decoded from
// bd's own JSON shape, not handed over as a Go struct with Labels filled in,
// so a read path that lost the array would fail this test.
func TestSpecCandidatesSkipsCrewOnlyFromBdJSON(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	crewed := `{"id":"gt-crew","title":"crew one","status":"open","priority":2,"issue_type":"bug","labels":["crew-only"]}`
	twin := `{"id":"gt-open","title":"twin","status":"open","priority":2,"issue_type":"bug"}`
	board := func(string) ([]*beads.Issue, error) {
		issues, _, err := beads.DecodeReadyBoard([]byte(`{"issues":[` + crewed + `,` + twin + `]}`))
		return issues, err
	}

	got := specCandidates(townRoot, 2, board)
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Spec.ID != "gt-open" {
		t.Errorf("candidates = %+v, want the unlabelled twin alone", got.Candidates)
	}
	if len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0].Line, specdispatch.CrewOnlyLabel) {
		t.Errorf("skipped = %+v, want a line naming %s", got.Skipped, specdispatch.CrewOnlyLabel)
	}
}

// jsonEscape escapes a string for the JSON test fixtures above.
func jsonEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func TestHasCommentWithPrefix(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{{Text: "unrelated"}, {Text: "  spec-dispatch: gt-a: routed to the planner (gt-4k3fj.7): x"}}
	if !hasCommentWithPrefix(comments, "spec-dispatch: gt-a: routed to the planner") {
		t.Error("existing planner note not found")
	}
	if hasCommentWithPrefix(comments, "spec-dispatch: gt-a: dispatch failed") {
		t.Error("different note kind matched")
	}
}

// The failure note is written once per reason: a retry that fails for another
// reason has to leave that reason on the bead, not the first one (gt-zkqtn).
func TestAnnotateSpecOnce_DispatchFailedDifferentReasonRecorded(t *testing.T) {
	t.Parallel()
	failNote := func(reason string) string {
		return fmt.Sprintf("%sgt-x: dispatch failed, left unassigned: %s", specDispatchNotePrefix, reason)
	}
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-x", Status: "open"})
	if err := db.AddComment("gt-x", failNote("reasonA")); err != nil {
		t.Fatal(err)
	}

	// The same failure on the next tick is still written once.
	if err := annotateSpecOnce(db, "gt-x", failNote("reasonA"), failNote("reasonA")); err != nil {
		t.Fatal(err)
	}
	// A different reason is a new fact about the bead.
	if err := annotateSpecOnce(db, "gt-x", failNote("reasonB"), failNote("reasonB")); err != nil {
		t.Fatal(err)
	}

	comments, err := db.Comments("gt-x")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range comments {
		got = append(got, c.Text)
	}
	want := []string{failNote("reasonA"), failNote("reasonB")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("comments = %q, want %q", got, want)
	}
}

// TestSpecReadyQuery pins the filters the dispatcher's board query carries;
// the argv they become is pinned in internal/beads (TestReadyFiltered).
func TestSpecReadyQuery(t *testing.T) {
	t.Parallel()
	opts := specReadyOptions()
	if !opts.Unassigned || opts.Limit != 0 {
		t.Errorf("ready options = %+v, want unassigned and the whole board (limit 0)", opts)
	}
	for _, want := range []string{"needs-human", "gt:ready-to-land", "gt:agent"} {
		if !slices.Contains(opts.ExcludeLabels, want) {
			t.Errorf("ready options exclude labels %q missing %q", opts.ExcludeLabels, want)
		}
	}
	for _, want := range []string{constants.EpicBeadType, "wisp"} {
		if !slices.Contains(opts.ExcludeTypes, want) {
			t.Errorf("ready options exclude types %q missing %q", opts.ExcludeTypes, want)
		}
	}
	// spec-dispatch-failed and crew-only are not excluded server-side: the
	// board carries the beads those labels hold so the tick can count them and
	// name them in its skip line, and Eligible keeps them out of the candidate
	// set (gt-q6zoo, gt-k1g3e).
	for _, carried := range []string{specdispatch.DispatchFailedLabel, specdispatch.CrewOnlyLabel} {
		if slices.Contains(opts.ExcludeLabels, carried) {
			t.Errorf("ready options exclude %s; the tick cannot report what it never reads", carried)
		}
	}
	// The retired label spec and type feature must not gate the board.
	if slices.Contains(opts.ExcludeLabels, "spec") || slices.Contains(opts.ExcludeTypes, "feature") {
		t.Errorf("ready options still carry the retired spec label or feature type: %+v", opts)
	}
	// The argv is fixed: the flags in this order, each exclusion list
	// comma-joined once, the whole board.
	want := []string{
		"ready", "--json", "--unassigned",
		"--exclude-label", strings.Join(opts.ExcludeLabels, ","),
		"--exclude-type", strings.Join(opts.ExcludeTypes, ","),
		"--limit", "0",
	}
	if got := beads.ReadyArgs(opts); !slices.Equal(got, want) {
		t.Errorf("ready argv = %q, want %q", got, want)
	}
}

// specReadyStub answers ReadyFiltered with a canned board and records the
// query it was asked. Only that method runs, so the rest of beads.Client stays
// unsatisfied.
type specReadyStub struct {
	beads.Client
	options beads.ReadyOptions
	issues  []*beads.Issue
	err     error
}

func (s *specReadyStub) ReadyFiltered(opts beads.ReadyOptions) ([]*beads.Issue, error) {
	s.options = opts
	return s.issues, s.err
}

// TestSpecReadyBoardReadsACutBoardAsPartial: a board bd reports as cut is the
// tick's board, not a failed tick. The sentinel says other rows may exist; the
// rows that did arrive are dispatched on.
func TestSpecReadyBoardReadsACutBoardAsPartial(t *testing.T) {
	t.Parallel()
	stub := &specReadyStub{
		issues: []*beads.Issue{{ID: "gt-a"}},
		err:    &beads.ErrReadyTruncated{Found: 100, Cap: 100, TrueCount: 373},
	}

	issues, err := specReadyBoardFrom(stub)
	if err != nil {
		t.Fatalf("specReadyBoardFrom on a cut board = %v, want the partial board and no error", err)
	}
	if len(issues) != 1 || issues[0].ID != "gt-a" {
		t.Errorf("specReadyBoardFrom = %v, want the rows the cut page carried", issues)
	}
	if !stub.options.Unassigned || stub.options.Limit != 0 {
		t.Errorf("board query = %+v, want the dispatcher's unassigned whole board", stub.options)
	}
}

// TestSpecReadyBoardReportsAFailedQuery: every other failure is the tick's, so
// the rig's error line names why the board could not be read.
func TestSpecReadyBoardReportsAFailedQuery(t *testing.T) {
	t.Parallel()
	stub := &specReadyStub{err: errors.New("bd ready: database locked")}

	if _, err := specReadyBoardFrom(stub); err == nil || !strings.Contains(err.Error(), "database locked") {
		t.Fatalf("specReadyBoardFrom err = %v, want bd's failure", err)
	}
}

// TestSpecDispatchDefersABeadHeldMidSling is the tick's half of gt-0k7kb: a
// hold can land while the sling is spawning, past every read the tick makes.
// The sling refuses at the claim (internal/sling), and the tick reads that
// while the sling is spawning, past every read the tick makes. The sling
// refuses at the claim (internal/sling, gt-0k7kb), and the tick reads that
// refusal as a deferral — skipped with a reason, left ready, no failure label
// for an operator to clear.
func TestSpecDispatchDefersABeadHeldMidSling(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{fmt.Errorf(
		"%s gt-a is held (label needs-human): the hold was written after this dispatch read the bead",
		dispatch.SlingRefusalMarker)}

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 {
		t.Fatalf("a bead held mid-sling was slung: %v", f.slung)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "sling deferred") {
		t.Fatalf("report = %+v, want one deferred skip", r)
	}
	if len(r.Failed) != 0 || len(f.notes["gt-a"]) != 0 {
		t.Fatalf("the refusal was handled as a failure: report %+v notes %v", r, f.notes)
	}
	if _, labeled := f.specs["gt-a"]; !labeled {
		t.Fatal("the bead left the fixture")
	}
	if got := f.specs["gt-a"]; got.HasLabel(specdispatch.DispatchFailedLabel) {
		t.Fatalf("the refused bead wears %s: it would leave the queue", specdispatch.DispatchFailedLabel)
	}
}

// TestSpecLintHelpNamesTheDaemonSection: the template default lives in the
// "daemon" section of settings/config.json once gt config migrate has run.
func TestSpecLintHelpNamesTheDaemonSection(t *testing.T) {
	t.Parallel()
	usage := specLintCmd.Flags().Lookup("template").Usage
	for name, text := range map[string]string{"Long": specLintCmd.Long, "--template": usage} {
		if strings.Contains(text, "daemon.json") {
			t.Errorf("gt spec lint %s help names the retired daemon.json: %q", name, text)
		}
		if !strings.Contains(text, "settings/config.json") {
			t.Errorf("gt spec lint %s help must name settings/config.json: %q", name, text)
		}
	}
}

// TestSpecDispatchHelpNamesTheOverseerLabels: the dispatch help names each
// overseer-first label the dispatcher skips, in every spelling it reads.
func TestSpecDispatchHelpNamesTheOverseerLabels(t *testing.T) {
	t.Parallel()
	// Match whole words, so the plain needs-overseer is not satisfied by
	// gt:needs-overseer.
	words := make(map[string]bool)
	punct := strings.NewReplacer(",", " ", ".", " ", "(", " ", ")", " ", ";", " ")
	for _, w := range strings.Fields(punct.Replace(specDispatchCmd.Long)) {
		words[w] = true
	}
	for _, want := range []string{
		"overseer-only",
		"operator-only",
		"needs-overseer",
		"gt:needs-overseer",
		"needs-operator",
		"gt:needs-operator",
	} {
		if !words[want] {
			t.Errorf("gt spec dispatch help does not name the label %q:\n%s", want, specDispatchCmd.Long)
		}
	}
}

// childrenFailBD is a store whose child read fails.
type childrenFailBD struct {
	beads.Client
	err error
}

func (b childrenFailBD) Children(string) ([]*beads.Issue, error) { return nil, b.err }

// TestOnceWriterDropsRepeats: the same settings failure, seen once per peer,
// reaches stderr once; a different warning still gets through.
func TestOnceWriterDropsRepeats(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	w := &onceWriter{w: &sink}
	for _, line := range []string{"settings bad\n", "settings bad\n", "children of gt-a\n", "settings bad\n"} {
		fmt.Fprint(w, line)
	}
	if got, want := sink.String(), "settings bad\nchildren of gt-a\n"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

// TestSpecPeerHeldFromWarnsOnReadFailure: a peer-hold question whose inputs will
// not read answers "not held" — the guard keeps refusing rather than guessing —
// but says so on stderr. A corrupt settings file used to disable every peer hold
// without a word.
func TestSpecPeerHeldFromWarnsOnReadFailure(t *testing.T) {
	t.Parallel()

	peer := sling.Duplicate{ID: "gt-peer", Title: "Fix TestFoo", Status: "open", Priority: 1, IssueType: "task",
		Ready: sling.Readiness{Description: specTestDescription, Acceptance: specTestAcceptance}}

	t.Run("corrupt town settings", func(t *testing.T) {
		t.Parallel()
		var warned bytes.Buffer
		townRoot := duplicateTown(t)
		writeModelsSettings(t, townRoot, "{not json")

		if got := specPeerHeldPinnedTo(&warned, townRoot, filepath.Join(townRoot, "gastown", "mayor", "rig"), peer, nil); got != "" {
			t.Errorf("unreadable settings held the peer: %q, want not held", got)
		}
		if out := warned.String(); !strings.Contains(out, "config.json") || !strings.Contains(out, "not held") {
			t.Errorf("warning = %q, want one naming the settings file and the not-held answer", out)
		}
	})

	t.Run("unreadable child set", func(t *testing.T) {
		t.Parallel()
		var warned bytes.Buffer
		knobs := specHoldKnobs{MaxPriority: 2, ShapeGate: specdispatch.ShapeGateOff, Warn: &warned}
		knobs.Backoff = func(string) string { return "" }

		store := childrenFailBD{err: errors.New("dial tcp 127.0.0.1:3307: connection refused")}
		if got := specPeerHeldFrom(store, knobs, peer, nil); got != "" {
			t.Errorf("an unreadable child set held the peer: %q, want not held", got)
		}
		if out := warned.String(); !strings.Contains(out, "gt-peer") || !strings.Contains(out, "connection refused") {
			t.Errorf("warning = %q, want one naming the peer and the read error", out)
		}
	})

	// A nil backoff reader is a caller that never wired the question, not a
	// bead with no record to read: the guard reads a resting head as next and
	// refuses its whole cluster, silently, so it has to say so (gt-mqa3r).
	t.Run("unwired backoff reader", func(t *testing.T) {
		t.Parallel()
		var warned bytes.Buffer
		knobs := specHoldKnobs{MaxPriority: 2, ShapeGate: specdispatch.ShapeGateOff, Warn: &warned}

		if got := specPeerHeldFrom(beadsfake.New(), knobs, peer, nil); got != "" {
			t.Errorf("a peer with no backoff reader was held: %q", got)
		}
		if out := warned.String(); !strings.Contains(out, "startup backoff") || !strings.Contains(out, "no backoff reader configured") {
			t.Errorf("warning = %q, want one naming the unanswered backoff question", out)
		}
	})

	t.Run("readable inputs stay silent", func(t *testing.T) {
		t.Parallel()
		var warned bytes.Buffer
		knobs := specHoldKnobs{MaxPriority: 2, ShapeGate: specdispatch.ShapeGateOff, Warn: &warned}
		knobs.Backoff = func(string) string { return "" }

		if got := specPeerHeldFrom(beadsfake.New(), knobs, peer, nil); got != "" {
			t.Errorf("a clean peer was held: %q", got)
		}
		if warned.Len() != 0 {
			t.Errorf("a clean read warned: %q", warned.String())
		}
	})
}

// TestSpecHoldKnobsWarnWriter: a knob set built without a writer still reports
// an unanswered question, because the answer it falls back to — "not held" —
// otherwise fails open in silence (gt-36qvt).
func TestSpecHoldKnobsWarnWriter(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	if got := (specHoldKnobs{Warn: &sink}).warnWriter(); got != &sink {
		t.Errorf("warnWriter() = %v, want the knob set's own writer", got)
	}
	if got := (specHoldKnobs{}).warnWriter(); got != specHoldWarn {
		t.Errorf("warnWriter() with no writer = %v, want the package's stderr writer", got)
	}
}

func TestSpecBudgetFromSeatMap(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{
		Timezone:    "UTC",
		MinSpawnGap: "4m",
		Seats: map[string]config.SeatConfig{
			"claude-sonnet":  {Cap: 3},
			"claude-haiku":   {Cap: 2},
			"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"},
		},
	}
	known := func(string) bool { return true }
	night := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	b, err := specBudgetFromSeats(ts, night, known)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Seats) != 3 || b.MinSpawnGap != 4*time.Minute {
		t.Fatalf("budget = %+v", b)
	}
	// Seats come in the loader's order, and only the shut one is closed.
	if got := b.Picture(); got != "claude-haiku 0/2, claude-sonnet 0/3, deepseek-flash 0/3 (closed until 08:00)" {
		t.Fatalf("picture = %q", got)
	}
	// A closed seat still counts its live polecats.
	b.SetLive(map[string]int{"deepseek-flash": 2})
	if flash := b.Seats[2]; flash.Window == nil || !flash.Window.Closed(night) || flash.Live != 2 || flash.FreeAt(night) {
		t.Fatalf("flash = %+v", flash)
	}

	day := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	b, err = specBudgetFromSeats(ts, day, known)
	if err != nil {
		t.Fatal(err)
	}
	if b.Seats[2].Window == nil || b.Seats[2].Window.Closed(day) || !b.Seats[2].FreeAt(day) {
		t.Fatalf("flash at noon = %+v, want open", b.Seats[2])
	}
}

// A seat's pinned_only mark reaches the tick's budget, so the dispatcher's
// unlabelled draw passes it over; a map that never sets the key leaves every
// seat unmarked (gt-zor86).
func TestSpecBudgetFromSeatMapCarriesPinnedOnly(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{
		Timezone: "UTC",
		Seats: map[string]config.SeatConfig{
			"claude-sonnet":  {Cap: 2},
			"deepseek-flash": {Cap: 1, PinnedOnly: true},
		},
	}
	known := func(string) bool { return true }
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	b, err := specBudgetFromSeats(ts, now, known)
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]bool{}
	for _, s := range b.Seats {
		pinned[s.Agent] = s.PinnedOnly
	}
	if len(b.Seats) != 2 || !pinned["deepseek-flash"] || pinned["claude-sonnet"] {
		t.Fatalf("pinned_only by seat = %v, want deepseek-flash only", pinned)
	}

	ts.PolecatPool.Seats["deepseek-flash"] = config.SeatConfig{Cap: 1}
	b, err = specBudgetFromSeats(ts, now, known)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range b.Seats {
		if s.PinnedOnly {
			t.Fatalf("seat %s is pinned-only with no pinned_only key", s.Agent)
		}
	}
}

// The tick's budget is built from one clock read, but the choice runs on a
// later one: a tick that starts at 19:59:58 on a seat whose window shuts at
// 20:00 must not sling onto it when the board read and the pre-sling holds
// carry the choice past the boundary (gt-t8new).
func TestSpecDispatchRechecksTheSeatWindow(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{
		Timezone: "UTC",
		Seats:    map[string]config.SeatConfig{"deepseek-flash": {Cap: 2, Closed: "20:00-08:00"}},
	}
	start := time.Date(2026, 10, 19, 19, 59, 58, 0, time.UTC)
	budget, err := specBudgetFromSeats(ts, start, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !budget.Seats[0].FreeAt(start) {
		t.Fatalf("at the tick's start the flash seat = %+v, want open", budget.Seats[0])
	}

	f := newFakeSpecTown(cleanSpec("gt-seat", 2, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.Budget = budget
	// The tick's clock advances a second on every read, so the reads before the
	// choice carry it across the boundary: the picture and the revert check run
	// at 19:59:58 and 19:59:59, and the choice runs at 20:00:00. Only a clock
	// re-read at the choice sees the window shut (gt-t8new).
	now := start
	env.Now = func() time.Time {
		t := now
		now = now.Add(time.Second)
		return t
	}
	r := runSpecDispatchCycle(env)

	if len(f.slung) != 0 {
		t.Fatalf("slung %v onto a seat shut before the choice, want a skip", f.slung)
	}
	found := false
	for _, skip := range r.Skipped {
		if skip.Bead == "gt-seat" && strings.Contains(skip.Line, "closed until 08:00") {
			found = true
		}
	}
	if !found {
		t.Fatalf("skipped = %+v, want the seat named closed", r.Skipped)
	}
}

// The window is read in polecat_pool.timezone and the picture shows the opening
// in that zone's wall clock, whatever zone the tick's clock is in (gt-r23nm).
func TestSpecBudgetFromSeatMapShowsTheWindowInThePoolTimezone(t *testing.T) {
	t.Parallel()
	known := func(string) bool { return true }
	for _, tc := range []struct {
		name, zone string
		now        time.Time
		closed     bool
		picture    string
	}{
		{
			// 03:00 UTC is 23:00 the evening before in New York (UTC-4): shut.
			name: "west of UTC, shut", zone: "America/New_York",
			now:     time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC),
			closed:  true,
			picture: "deepseek-flash 0/3 (closed until 08:00)",
		},
		{
			// 03:00 UTC is 12:00 in Tokyo (UTC+9): open, though 03:00 UTC is
			// inside 20:00-08:00 read as UTC.
			name: "east of UTC, open", zone: "Asia/Tokyo",
			now:     time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC),
			closed:  false,
			picture: "deepseek-flash 0/3",
		},
		{
			// 12:00 UTC is 21:00 in Tokyo: shut, though noon UTC is open read as UTC.
			name: "east of UTC, shut", zone: "Asia/Tokyo",
			now:     time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
			closed:  true,
			picture: "deepseek-flash 0/3 (closed until 08:00)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := config.NewTownSettings()
			ts.PolecatPool = &config.PolecatPool{
				Timezone: tc.zone,
				Seats:    map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"}},
			}
			b, err := specBudgetFromSeats(ts, tc.now, known)
			if err != nil {
				t.Fatal(err)
			}
			if got := b.Seats[0].Window != nil && b.Seats[0].Window.Closed(tc.now); got != tc.closed {
				t.Errorf("Closed at the tick = %v, want %v", got, tc.closed)
			}
			if got := b.Picture(); got != tc.picture {
				t.Errorf("picture = %q, want %q", got, tc.picture)
			}
		})
	}
}

func TestSpecBudgetFromSeatMapRejectsWhatTheLoaderRejects(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{"nope": {Cap: 1}}}
	_, err := specBudgetFromSeats(ts, time.Now(), config.AgentResolver("", ts))
	if err == nil || !strings.Contains(err.Error(), "polecat_pool.seats.nope") {
		t.Fatalf("err = %v, want the loader's message naming the seat", err)
	}
}

// The seat map is the whole list: the tick's budget names the agents the map
// holds, and a map emptied of its seats leaves the dispatcher with none rather
// than a default agent to spend (gt-mukgk).
func TestSpecBudgetForIsTheSeatMapAlone(t *testing.T) {
	t.Parallel()
	ts := specSeatTown(map[string]config.SeatConfig{"claude-haiku": {Cap: 1}})
	got, err := specBudgetFor("", ts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Picture() != "claude-haiku 0/1" {
		t.Fatalf("picture = %q", got.Picture())
	}

	ts.PolecatPool.Seats = nil
	got, err = specBudgetFor("", ts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Picture() != "no seats" {
		t.Fatalf("an emptied map = %q, want no seats", got.Picture())
	}
}

// A polecat that records no agent is listed in the tick's notices, sorted, so
// the operator sees which sessions fill no seat (gt-mukgk). A roster with none
// says nothing.
func TestSpecDispatchNoticesPolecatsWithNoRecordedAgent(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-plain", 1, "2026-09-29T10:00:00Z"))
	f.roster = specRoster{
		Live:       map[string]int{config.UnknownSeatAgent: 2},
		Unrecorded: []string{"pearl", "onyx"},
	}
	r := runSpecDispatchCycle(f.env())
	if len(r.Notices) != 1 ||
		!strings.Contains(r.Notices[0], "2 polecat(s) record no agent") ||
		!strings.Contains(r.Notices[0], "onyx, pearl") {
		t.Fatalf("notices = %v, want one naming the two polecats, sorted", r.Notices)
	}

	f = newFakeSpecTown(cleanSpec("gt-plain", 1, "2026-09-29T10:00:00Z"))
	f.roster = specRoster{Live: map[string]int{"claude-sonnet": 1}}
	if r := runSpecDispatchCycle(f.env()); len(r.Notices) != 0 {
		t.Fatalf("notices = %v, want none when every polecat records its agent", r.Notices)
	}
}

// TestSpecRosterFrom_ParkedStaleSeatIsListedNotCounted is gt-t7k92 on the
// roster: a released parked seat is not in Live, and the page names it next to
// the dead-hooked seat that still is.
func TestSpecRosterFrom_ParkedStaleSeatIsListedNotCounted(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}}}
	sessions := []poolSession{
		{name: "live/ember", agent: "deepseek-flash"},
		{name: "dead/gastown/jade", agent: "deepseek-flash", deadHooked: true},
		{name: "dead/gastown/mariner", agent: "deepseek-flash", parkedStale: true},
	}
	r := specRosterFrom(sessions, ts)
	if r.Live["deepseek-flash"] != 2 || r.DeadHooked["deepseek-flash"] != 1 || r.ParkedStale["deepseek-flash"] != 1 {
		t.Fatalf("roster = live %v, dead-hooked %v, parked-stale %v, want 2, 1 and 1", r.Live, r.DeadHooked, r.ParkedStale)
	}
	live := poolShowLive{live: r.Live, dead: r.DeadHooked, parked: r.ParkedStale, parkedTotal: 1, total: 2, ok: true}
	text := live.seatCount("deepseek-flash", 3)
	for _, want := range []string{"2/3", "(1 dead-hooked)", "(1 parked-stale)"} {
		if !strings.Contains(text, want) {
			t.Errorf("seat count %q does not contain %q", text, want)
		}
	}
	cap := 3
	if total := live.totalCount(&cap); total != "2/3 (1 parked-stale)" {
		t.Errorf("total count = %q, want %q", total, "2/3 (1 parked-stale)")
	}
	live.parkedTotal = 0
	if total := live.totalCount(&cap); total != "2/3" {
		t.Errorf("total count with nothing released = %q, want %q", total, "2/3")
	}
}

// TestSpecHoldFilesHonorsTheReadOnlyMarkerThroughTheRealExtraction drives the
// call shape the dispatcher uses: refs extracted from the bead's fields, then
// specHoldFiles with the text those refs carry. A path marked (read-only) at
// every mention drops out of the hold; the same path unmarked, or marked at
// only one of two mentions, stays held (gt-5v13p).
func TestSpecHoldFilesHonorsTheReadOnlyMarkerThroughTheRealExtraction(t *testing.T) {
	t.Parallel()
	const work, shared = "internal/foo/foo.go", "internal/bar/shared.go"
	body := func(constraints string) string {
		return "## Goal\nChange the thing.\n\n## Constraints\n" + constraints + "\n\n## Out of scope\nNothing.\n"
	}
	tests := []struct {
		name        string
		constraints string
		notes       string
		want        []string
	}{
		{"marked path drops out", "Change " + work + ". Reads " + shared + " (read-only).", "", []string{work}},
		{"unmarked path stays held", "Change " + work + ". Reads " + shared + ".", "", []string{shared, work}},
		{"a second mention without the marker keeps it held", "Change " + work + ". Reads " + shared + " (read-only).", "Also edits " + shared + ".", []string{shared, work}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			description := body(tc.constraints)
			refs := extractOverlapRefs("a bead", description, "", tc.notes)
			got := specHoldFiles(refs.Files, description, refs.Text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("specHoldFiles = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpecHoldReadOnlyMarkerReadsEveryTextField drives the marker through the
// two production hold sites — heldBeadFrom for a seat's held bead and
// specPeerHeldBySeat for the guard's peer question — with a path marked
// (read-only) in each text field the
// refs are extracted from. A site that stops passing its text, or an extraction
// that stops reading a field, leaves the path held and turns that subtest red;
// its unmarked twin shows the pair would otherwise hold (gt-dilzl, gt-5v13p).
func TestSpecHoldReadOnlyMarkerReadsEveryTextField(t *testing.T) {
	t.Parallel()
	const shared = "internal/cmd/spec.go"
	const seat = "gastown/polecats/capable"

	type fields struct{ title, description, design, notes string }
	// named is a bead that names shared in field, with the marker when marked
	// is set. The description keeps a parsed, non-empty Constraints section:
	// specHoldFiles reads the marker only where one exists (gt-zs8fq).
	named := func(field string, marked bool) fields {
		f := fields{
			title:       "Work on the thing",
			description: "## Goal\nChange the thing.\n\n## Constraints\nTouch nothing else.\n",
			design:      "Root cause: a stale read.",
			notes:       "No notes yet.",
		}
		mention := "Read " + shared
		if marked {
			mention += " (read-only)"
		}
		mention += " first."
		switch field {
		case "title":
			f.title += "; " + mention
		case "description":
			f.description += "\n" + mention + "\n"
		case "design":
			f.design += " " + mention
		case "notes":
			f.notes += " " + mention
		}
		return f
	}
	// twoBeads reduces both beads through the production pool builder, so the
	// refs and the text each site reads come from the extraction the
	// dispatcher's own reads use, not from fields this test fills in.
	twoBeads := func(t *testing.T, held, peer fields) []duplicateCandidate {
		t.Helper()
		db := beadsfake.New()
		db.Seed(beads.Issue{ID: "gt-held", Status: beads.StatusHooked, Assignee: seat, Priority: 2,
			Title: held.title, Description: held.description, Design: held.design, Notes: held.notes})
		db.Seed(beads.Issue{ID: "gt-peer", Status: beads.StatusHooked, Priority: 2,
			Title: peer.title, Description: peer.description, Design: peer.design, Notes: peer.notes})
		got, err := listDuplicateCandidates(db, func(string, error) {}, t.TempDir(), []string{beads.StatusHooked}, time.Time{})
		if err != nil {
			t.Fatalf("listDuplicateCandidates: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("pool = %+v, want both beads", got)
		}
		return got
	}
	fromPool := func(t *testing.T, pool []duplicateCandidate, id string) duplicateCandidate {
		t.Helper()
		for _, c := range pool {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("pool %+v has no %s", pool, id)
		return duplicateCandidate{}
	}
	holds := func(paths []string) bool { return slices.Contains(paths, shared) }

	sites := []struct {
		name  string
		stays func(t *testing.T, field string, marked bool) bool
	}{
		{
			name: "heldBeadFrom",
			stays: func(t *testing.T, field string, marked bool) bool {
				t.Helper()
				held, ok := heldBeadFrom(fromPool(t, twoBeads(t, named(field, marked), named(field, false)), "gt-held"))
				if !ok {
					t.Fatal("heldBeadFrom refused a bead a polecat seat holds")
				}
				return holds(held.Paths)
			},
		},
		{
			name: "specPeerHeldBySeat/peer marks",
			stays: func(t *testing.T, field string, marked bool) bool {
				t.Helper()
				pool := twoBeads(t, named(field, false), named(field, marked))
				return specPeerHeldBySeat(fromPool(t, pool, "gt-peer"), pool) != ""
			},
		},
		{
			name: "specPeerHeldBySeat/held marks",
			stays: func(t *testing.T, field string, marked bool) bool {
				t.Helper()
				pool := twoBeads(t, named(field, marked), named(field, false))
				return specPeerHeldBySeat(fromPool(t, pool, "gt-peer"), pool) != ""
			},
		},
	}
	for _, site := range sites {
		for _, field := range []string{"title", "description", "design", "notes"} {
			t.Run(site.name+"/"+field+"/marked", func(t *testing.T) {
				t.Parallel()
				if site.stays(t, field, true) {
					t.Errorf("%s marked (read-only) in the %s still holds at %s", shared, field, site.name)
				}
			})
			t.Run(site.name+"/"+field+"/unmarked", func(t *testing.T) {
				t.Parallel()
				if !site.stays(t, field, false) {
					t.Errorf("%s named without the marker does not hold at %s, so the marked case proves nothing", shared, site.name)
				}
			})
		}
	}
}

// Two open beads that name one test and one file both dispatch in one tick: the
// tick holds neither for the other, and says nothing about a hold or a
// deferral. Landings are serial per rig and a rebase conflict costs a bounce, so
// the overlap is not worth an idle seat (gt-e1ufo).
func TestSpecDispatchSlingsEveryBeadOfAnOverlappingCluster(t *testing.T) {
	t.Parallel()
	cluster := func(id, created string) specdispatch.Spec {
		s := cleanSpec(id, 2, created)
		s.Title = "host rehearsal: the red tier sweep fails (TestHostRehearsal)"
		s.Description = specTestDescription + "\n\nThe fix lands in internal/cmd/spec.go.\n"
		return s
	}
	f := newFakeSpecTown(
		cluster("gt-first", "2026-10-25T18:40:00Z"),
		cluster("gt-second", "2026-10-25T18:41:00Z"),
	)
	env := f.env()
	env.PerTick = 2
	r := runSpecDispatchCycle(env)

	slung := map[string]bool{}
	for _, id := range f.slung {
		slung[id] = true
	}
	if len(f.slung) != 2 || !slung["gt-first"] || !slung["gt-second"] {
		t.Fatalf("slung %v, want both beads of the overlapping cluster in one tick; report %+v", f.slung, r)
	}
	if len(r.Skipped) != 0 || len(r.Deferred) != 0 {
		t.Fatalf("skipped = %+v, deferred = %+v; want no hold or deferral line", r.Skipped, r.Deferred)
	}
}

// The spec dispatcher's sling admits a content overlap instead of refusing it,
// because the tick slings every ready bead it reaches (gt-e1ufo). The manual
// sling never builds these params, so it keeps the refusal.
func TestSpecSlingParamsAdmitOverlap(t *testing.T) {
	t.Parallel()
	c := specCandidate{Spec: specdispatch.Spec{ID: "gt-a"}, Rig: "gastown"}
	p := specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", c, "", specdispatch.SeatChoice{Agent: "gastown/polecats/capable"})
	if !p.AdmitOverlap {
		t.Fatal("the spec dispatcher's sling params do not admit a content overlap")
	}
}

// The tick's params reach the decision through sling.Run, not a fake: the
// overlap that refused a manual sling hooks the bead once the dispatcher's
// params carry it (gt-rd213, gt-e1ufo). Clearing AdmitOverlap on the same
// params brings the refusal back, so the flag is what the engine acts on.
func TestSpecSlingParamsDriveSlingRunPastAContentOverlap(t *testing.T) {
	t.Parallel()
	const beadID = "gt-overlap"
	// A bead hooked to a working seat, sharing the candidate's test: the
	// overlap Blocking refuses whoever the candidate is.
	overlap := func(string, string, *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
		return &duplicateCandidate{ID: beadID, Status: "open", Priority: 2},
			[]duplicateMatch{{
				Bead:        duplicateCandidate{ID: "gt-seat", Status: "hooked"},
				SharedTests: []string{"TestHostRehearsal"},
			}}, nil
	}
	for _, tc := range []struct {
		name        string
		admit       bool
		wantRefused bool
	}{
		{"the tick's params admit the overlap", true, false},
		{"the same sling without AdmitOverlap is refused", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(beadID, beadInfo{Status: "open", Title: "the red tier sweep fails (TestHostRehearsal)"})
			h.run.slingDeps.checkDuplicates = overlap

			townRoot := t.TempDir()
			params := specSlingParams(townRoot, filepath.Join(townRoot, "gastown", ".beads"), "mol-polecat-work",
				specCandidate{Spec: specdispatch.Spec{ID: beadID}, Rig: "gastown"}, "",
				specdispatch.SeatChoice{Agent: "gastown/polecats/frost"})
			// A manual sling builds its own params and leaves the flag zero;
			// the tick's are used as specSlingParams made them.
			if !tc.admit {
				params.AdmitOverlap = false
			}

			_, err := sling.Run(context.Background(), h.run.slingDeps.engineDeps(), params)

			if refused := errors.Is(err, sling.ErrDuplicateContent); refused != tc.wantRefused {
				t.Fatalf("refused = %v (err %v), want %v", refused, err, tc.wantRefused)
			}
			bead := h.bead(beadID)
			if bead == nil {
				t.Fatalf("the sling lost the bead %s", beadID)
			}
			if hooked := bead.Status == "hooked"; hooked == tc.wantRefused {
				t.Fatalf("bead %s is %q after the sling, want hooked = %v", beadID, bead.Status, !tc.wantRefused)
			}
		})
	}
}
