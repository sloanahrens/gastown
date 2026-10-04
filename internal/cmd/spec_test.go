package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/steward"
)

const specTestDescription = "## Goal\nA thing.\n\n## Constraints\nGo.\n\n## Out of scope\nNone.\n\n## Gate\nmake gate\n\n## Size\none worker, one MR"

// cleanSpec is a plain work bead: type task, no labels. Shape is the whole
// gate, so it must be dispatchable as it stands (gt-mmsr2).
func cleanSpec(id string, priority int, created string) specdispatch.Spec {
	return specdispatch.Spec{
		ID: id, Title: "Add " + id, Type: "task", Status: "open", Priority: priority, CreatedAt: created,
		Description: specTestDescription, Acceptance: "- [ ] a\n- [ ] b\n- [ ] c",
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
	revert    map[string]*specdispatch.Revert
	// children is each bead's direct child set, and childErrs a per-bead
	// children read failure, for the container rule.
	children map[string][]specdispatch.Child
	// showOverride is the full read the tick sees when it differs from the
	// board snapshot: the board lagged a note the read carries (gt-kr5xv).
	// An id absent here falls back to specs, so the common case needs no entry.
	showOverride map[string]specdispatch.Spec
	// showSeq is what a bead's successive reads return, consumed in order with
	// the last entry repeating: the first read is the tick's re-check of the
	// board snapshot, the one before the sling is the last-moment re-read
	// (gt-01gix). An id absent here reads from showOverride, then specs.
	showSeq    map[string][]specRead
	childErrs  map[string]error
	slingErrs  map[string][]error // per bead, consumed in order
	slung      []string
	slingSeats []specdispatch.SeatChoice
	// slungResume is the resume branch each sling carried, positionally with
	// slung: "" when the dispatch started fresh.
	slungResume []string
	// originBranches is what BranchOnOrigin finds, keyed "rig/branch"; an
	// absent key is a branch that is gone from origin.
	originBranches map[string]bool
	originErr      error
	notes          map[string][]string
	labels         map[string][]string
	sleeps         int
	// seats overrides the tick's budget seats; nil is the default pool, which
	// reserves no label and so holds a needs-pro bead (gt-lxxo4).
	seats []specdispatch.Seat
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

func newFakeSpecTown(specs ...specdispatch.Spec) *fakeSpecTown {
	f := &fakeSpecTown{specs: map[string]specdispatch.Spec{}, rigHold: map[string]string{}, revert: map[string]*specdispatch.Revert{},
		slingErrs: map[string][]error{}, originBranches: map[string]bool{}, notes: map[string][]string{}, labels: map[string][]string{},
		children: map[string][]specdispatch.Child{}, childErrs: map[string]error{}, showSeq: map[string][]specRead{}}
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
	budget := specdispatch.Budget{Seats: seats}
	return specDispatchEnv{
		Hold: func() string { return f.hold },
		Candidates: func() specBoardRead {
			var list []specdispatch.Spec
			labeled := 0
			for _, s := range f.specs {
				if s.HasLabel(specdispatch.DispatchFailedLabel) {
					labeled++
					continue
				}
				if ok, _ := specdispatch.Eligible(s, 2, budget.ReservedLabels()); ok {
					list = append(list, s)
				}
			}
			specdispatch.Order(list)
			out := make([]specCandidate, 0, len(list))
			for _, s := range list {
				out = append(out, specCandidate{Spec: s, Rig: "gastown"})
			}
			return specBoardRead{Candidates: out, LabeledFailed: labeled}
		},
		Show: func(id string) (specdispatch.Spec, error) {
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
		RigHold:        func(rig string) string { return f.rigHold[rig] },
		RevertInFlight: func(rig string) *specdispatch.Revert { return f.revert[rig] },
		BranchOnOrigin: func(rig, branch string) (bool, error) {
			if f.originErr != nil {
				return false, f.originErr
			}
			return f.originBranches[rig+"/"+branch], nil
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
			if errs := f.slingErrs[c.Spec.ID]; len(errs) > 0 {
				f.slingErrs[c.Spec.ID] = errs[1:]
				if errs[0] != nil {
					return "", errs[0]
				}
			}
			f.slung = append(f.slung, c.Spec.ID)
			f.slingSeats = append(f.slingSeats, seat)
			f.slungResume = append(f.slungResume, resumeBranch)
			s := f.specs[c.Spec.ID]
			s.Status, s.Assignee = "hooked", "gastown/polecats/p"
			f.specs[c.Spec.ID] = s
			return "p", nil
		},
		Sleep:    func(time.Duration) { f.sleeps++ },
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
	// The first seat until its cap (2), then the next seat.
	if f.slingSeats[0].Agent != "deepseek-flash" || f.slingSeats[1].Agent != "deepseek-flash" || f.slingSeats[2].Agent != "claude-sonnet" {
		t.Errorf("seats = %+v", f.slingSeats)
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

// needs-pro is the pro seat's selector, so the tick routes the bead there
// instead of reading the shared rule's hold on it; with no seat reserving the
// label, the hold stands (gt-lxxo4).
func TestSpecDispatchRoutesNeedsProToItsSeat(t *testing.T) {
	t.Parallel()
	pro := cleanSpec("gt-pro", 1, "2026-09-29T10:00:00Z")
	pro.Labels = []string{"needs-pro"}

	t.Run("a reserving seat takes it", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(pro)
		f.seats = []specdispatch.Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "deepseek-pro", Cap: 1, Label: "needs-pro"},
		}
		r := runSpecDispatchCycle(f.env())
		if len(f.slung) != 1 || f.slung[0] != "gt-pro" {
			t.Fatalf("slung %v skipped %+v, want the needs-pro bead routed", f.slung, r.Skipped)
		}
		if len(f.slingSeats) != 1 || f.slingSeats[0].Agent != "deepseek-pro" {
			t.Fatalf("seats %+v, want the pro seat", f.slingSeats)
		}
	})

	t.Run("no reserving seat holds it", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(pro)
		r := runSpecDispatchCycle(f.env())
		if len(f.slung) != 0 || len(r.Dispatched) != 0 {
			t.Fatalf("slung %v report %+v with no seat reserving needs-pro", f.slung, r)
		}
	})
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

// TestSpecDispatchReportsPlanState: the tick says which of the two planning
// states a spec is in — waiting for a plan job's proposal, or carrying one the
// operator files — instead of naming a planner that is not built (gt-4k3fj.14).
func TestSpecDispatchReportsPlanState(t *testing.T) {
	t.Parallel()
	waiting := cleanSpec("gt-wait", 1, "2026-09-29T10:00:00Z")
	waiting.Labels = append(waiting.Labels, "needs-planning")
	proposed := cleanSpec("gt-done", 1, "2026-09-29T11:00:00Z")
	proposed.Labels = append(proposed.Labels, "needs-planning")
	proposed.Notes = steward.PlanProposalMarker + "\nSpec: gt-done\nChildren: 2\n"
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

func TestSpecDispatchRetriesDoltContention(t *testing.T) {
	t.Parallel()
	contention := errors.New("formula failed: bd mol bond: Error 1213 (40001): serialization failure")

	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{contention, contention}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.sleeps != 2 || !strings.Contains(r.Dispatched[0].Line, "after 3 attempts") {
		t.Fatalf("slung %v sleeps %d report %+v", f.slung, f.sleeps, r.Dispatched)
	}

	f = newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	for i := 0; i < 10; i++ {
		f.slingErrs["gt-a"] = append(f.slingErrs["gt-a"], contention)
	}
	r = runSpecDispatchCycle(f.env())
	runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Failed) != 1 || f.specs["gt-a"].Assignee != "" {
		t.Fatalf("final contention failure: slung %v failed %+v", f.slung, r.Failed)
	}
	if !strings.Contains(r.Failed[0].Line, "left unassigned") || !strings.Contains(r.Failed[0].Line, "after 4 attempts") {
		t.Errorf("failure line = %q", r.Failed[0].Line)
	}
	if n := len(f.notes["gt-a"]); n != 1 {
		t.Errorf("annotated %d times across two failing ticks, want once", n)
	}
	if got := f.labels["gt-a"]; len(got) != 1 || got[0] != specdispatch.DispatchFailedLabel {
		t.Errorf("failed bead labels = %v, want %s once (the second tick must not retry it)", got, specdispatch.DispatchFailedLabel)
	}
}

func TestSpecDispatchPoolRefusalIsASkip(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{&poolBackpressureError{Reason: "pool: overflow full (2/2) -> no seat"}}
	r := runSpecDispatchCycle(f.env())
	if len(r.Skipped) != 1 || len(r.Failed) != 0 || len(f.notes["gt-a"]) != 0 || f.sleeps != 0 {
		t.Fatalf("pool refusal handled as failure: %+v notes %v", r, f.notes)
	}
}

// A content-overlap refusal is a deferral, not a failure: the guard refuses
// only live work, so the refusal clears by itself when the overlapping bead
// closes and the bead is retried with no label for an operator to remove
// (gt-q6zoo).
func TestSpecDispatchOverlapRefusalDefersAndRetries(t *testing.T) {
	t.Parallel()
	overlap := sling.DecideDuplicates("gt-a", []sling.DuplicateMatch{{
		Bead:        sling.Duplicate{ID: "gt-b", Status: "open"},
		SharedTests: []string{"TestSpecDispatch"},
	}}).Err()
	if !errors.Is(overlap, errSlingDuplicateContent) {
		t.Fatalf("fixture: %v is not the overlap sentinel", overlap)
	}

	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{overlap}
	r := runSpecDispatchCycle(f.env())
	if len(r.Failed) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("overlap refusal handled as a failure: %+v", r)
	}
	if !strings.Contains(r.Skipped[0].Line, "content overlap") {
		t.Errorf("deferral line = %q, want the overlap named", r.Skipped[0].Line)
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("overlap-refused bead labeled %v; the label would strand it out of the queue", got)
	}
	if n := len(f.notes["gt-a"]); n != 0 {
		t.Errorf("overlap-refused bead annotated %d times, want none", n)
	}

	// The overlapping bead closes, the guard stops refusing, and the next tick
	// dispatches the bead with nothing removed by hand.
	r = runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slung[0] != "gt-a" {
		t.Fatalf("slung %v, want gt-a retried on the next tick", f.slung)
	}
	if len(f.labels["gt-a"]) != 0 {
		t.Errorf("labels after the retry = %v, want none", f.labels["gt-a"])
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

func TestSpecBudgetFromConfig(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.RoleAgents = map[string]string{"polecat": "deepseek-flash"}
	ts.PolecatPool = &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 3, MinSpawnGap: "4m"}

	// The pool's seats: the overflow seat at max_overflow, then the pro seat at
	// its own cap. No claude-sonnet seat is in the budget (gt-4k3fj.8.8).
	b := specBudgetFromConfig(ts, nil)
	if got := b.Picture(); got != "deepseek-flash 0/3, deepseek-pro 0/1" || b.MinSpawnGap != 4*time.Minute {
		t.Fatalf("defaults = %q gap %v", got, b.MinSpawnGap)
	}
	if b.Seats[1].Label != "needs-pro" {
		t.Errorf("pro seat selector = %q, want needs-pro", b.Seats[1].Label)
	}

	// The pro seat's keys are the operator's: pro_max caps it (0 drops it
	// entirely), pro_agent picks the agent, pro_label its selector.
	ts.PolecatPool.ProMax = intPtr(2)
	ts.PolecatPool.ProAgent = "deepseek-reasoner"
	ts.PolecatPool.ProLabel = "hard"
	b = specBudgetFromConfig(ts, nil)
	if got := b.Picture(); got != "deepseek-flash 0/3, deepseek-reasoner 0/2" || b.Seats[1].Label != "hard" {
		t.Fatalf("pro seat = %q selector %q", got, b.Seats[1].Label)
	}
	ts.PolecatPool.ProMax = intPtr(0)
	if got := specBudgetFromConfig(ts, nil).Picture(); got != "deepseek-flash 0/3" {
		t.Fatalf("pro_max 0 must drop the seat: %q", got)
	}
	ts.PolecatPool.ProMax = nil

	// max_overflow unset: the dispatcher caps the seat itself rather than
	// treating the pool's silence as unlimited room.
	ts.PolecatPool.MaxOverflow = 0
	if got := specBudgetFromConfig(ts, nil).Picture(); got != "deepseek-flash 0/2, deepseek-reasoner 0/1" {
		t.Fatalf("unset max_overflow = %q", got)
	}
	ts.PolecatPool.MaxOverflow = 3
	ts.PolecatPool.ProAgent, ts.PolecatPool.ProLabel = "", ""
}

// The hooked seat is off unless spec_dispatch names a cap above zero, even in
// a town whose patrol is enabled: a seat nobody asked for is a seat the
// dispatcher must not spend (gt-4k3fj.8.8 acceptance 3).
func TestSpecBudgetHookedSeatIsOptIn(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 3}

	enabled := &config.SpecDispatchConfig{Enabled: true}
	for _, sd := range []*config.SpecDispatchConfig{nil, enabled, {Enabled: true, MaxHooked: -1}} {
		if got := specBudgetFromConfig(ts, sd).Picture(); got != "deepseek-flash 0/3, deepseek-pro 0/1" {
			t.Errorf("sd %+v: a hooked seat appeared without max_hooked: %q", sd, got)
		}
	}

	// An explicit cap adds it after the pool's seats (default agent).
	if got := specBudgetFromConfig(ts, &config.SpecDispatchConfig{MaxHooked: 1}).Picture(); got != "deepseek-flash 0/3, deepseek-pro 0/1, claude-sonnet 0/1" {
		t.Errorf("max_hooked 1 = %q", got)
	}
	// prefer_hooked moves it first, and hooked_agent names its agent.
	sd := &config.SpecDispatchConfig{MaxHooked: 2, PreferHooked: true, HookedAgent: "claude-opus"}
	if got := specBudgetFromConfig(ts, sd).Picture(); got != "claude-opus 0/2, deepseek-flash 0/3, deepseek-pro 0/1" {
		t.Errorf("prefer_hooked = %q", got)
	}
	// hooked_agent naming an existing seat keeps that seat once.
	sd = &config.SpecDispatchConfig{MaxHooked: 2, HookedAgent: "deepseek-flash"}
	if got := specBudgetFromConfig(ts, sd).Picture(); got != "deepseek-flash 0/3, deepseek-pro 0/1" {
		t.Errorf("duplicate seat = %q", got)
	}
}

// A pool with no overflow_agent has no seat for the dispatcher to fill: the
// pool's own admission point says the same (choosePoolAgent).
func TestSpecBudgetWithoutAPoolSeat(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	if got := specBudgetFromConfig(ts, nil).Picture(); got != "no seats" {
		t.Errorf("nil pool = %q, want no seats", got)
	}
	ts.PolecatPool = &config.PolecatPool{MaxOverflow: 3}
	if got := specBudgetFromConfig(ts, nil).Picture(); got != "deepseek-pro 0/1" {
		t.Errorf("pool without overflow_agent = %q", got)
	}
}

func intPtr(n int) *int { return &n }

func TestSpecRosterCountsByAgent(t *testing.T) {
	t.Parallel()
	ts := config.NewTownSettings()
	ts.RoleAgents = map[string]string{"polecat": "deepseek-flash"}
	now := time.Now()
	r := specRosterFrom([]poolSession{
		{name: "a", agent: "claude-sonnet", created: now.Add(-time.Hour)},
		{name: "b", agent: "deepseek-flash", created: now.Add(-2 * time.Hour)},
		{name: "c", agent: "", created: now.Add(-time.Minute)}, // role default
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
	ts.RoleAgents = map[string]string{"polecat": "deepseek-flash"}
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

	got := specCandidates(townRoot, 2, fakeBoard, nil)
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
	got = specCandidates(townRoot, 4, fakeBoard, nil)
	if ids := idList(got.Candidates); ids != "gt-bug gt-task gt-feature gt-p4" {
		t.Errorf("candidates at a P4 ceiling = %q", ids)
	}
	if got.LabeledFailed != 1 {
		t.Errorf("LabeledFailed at a P4 ceiling = %d, want 1", got.LabeledFailed)
	}
}

// capturedSpecBoard is the board the dispatcher reads, as the dispatcher reads
// it: the bytes of testdata/spec_ready_labels.json — a `bd ready --json`
// response captured from the live gastown rig on 2026-10-02 with the exact
// args specReadyArgs builds — through the production decoder parseSpecReady.
//
// The fakes elsewhere in this file hand the decoder a slice of beads.Issue
// values with Labels filled in, so they cannot catch a read path that never
// carries labels at all. This one starts from bd's own JSON, which is where
// om's critical finding lived (gt-q6zoo attempt 1).
func capturedSpecBoard(t *testing.T) specBoard {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "spec_ready_labels.json"))
	if err != nil {
		t.Fatalf("captured board fixture: %v", err)
	}
	return func(string) ([]*beads.Issue, error) { return parseSpecReady(data) }
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

	got := specCandidates(townRoot, 3, capturedSpecBoard(t), nil)
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

func TestSpecReadyQueryAndParse(t *testing.T) {
	t.Parallel()
	args := strings.Join(specReadyArgs(), " ")
	for _, want := range []string{"ready --json", "--unassigned", "--limit 0", "needs-human", "gt:ready-to-land", "--exclude-type", "epic,wisp", "gt:agent"} {
		if !strings.Contains(args, want) {
			t.Errorf("ready args %q missing %q", args, want)
		}
	}
	// spec-dispatch-failed is not excluded server-side: the board carries the
	// beads the label holds so the tick can count them, and Eligible keeps
	// them out of the candidate set (gt-q6zoo).
	if strings.Contains(args, "spec-dispatch-failed") {
		t.Errorf("ready args %q still exclude spec-dispatch-failed; the tick cannot count what it never reads", args)
	}
	// The retired label spec and type feature must not gate the board.
	for _, unwanted := range []string{"--label spec", "--type feature"} {
		if strings.Contains(args, unwanted) {
			t.Errorf("ready args %q still carry the retired %q", args, unwanted)
		}
	}
	for _, in := range []string{`[{"id":"gt-a","issue_type":"feature","status":"open"}]`, `{"issues":[{"id":"gt-a","issue_type":"feature","status":"open"}]}`} {
		got, err := parseSpecReady([]byte(in))
		if err != nil || len(got) != 1 || got[0].ID != "gt-a" {
			t.Errorf("parseSpecReady(%s) = %v, %v", in, got, err)
		}
	}
	if got, err := parseSpecReady([]byte("")); err != nil || got != nil {
		t.Errorf("empty = %v %v", got, err)
	}
}
