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
	"github.com/steveyegge/gastown/internal/specdispatch"
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
	specs      map[string]specdispatch.Spec
	order      []string
	roster     specRoster
	rosterErr  error
	hold       string
	rigHold    map[string]string
	slingErrs  map[string][]error // per bead, consumed in order
	slung      []string
	slingSeats []specdispatch.SeatChoice
	notes      map[string][]string
	labels     map[string][]string
	sleeps     int
}

func newFakeSpecTown(specs ...specdispatch.Spec) *fakeSpecTown {
	f := &fakeSpecTown{specs: map[string]specdispatch.Spec{}, rigHold: map[string]string{}, slingErrs: map[string][]error{},
		notes: map[string][]string{}, labels: map[string][]string{}}
	for _, s := range specs {
		f.specs[s.ID] = s
	}
	return f
}

func (f *fakeSpecTown) env() specDispatchEnv {
	return specDispatchEnv{
		Hold: func() string { return f.hold },
		Candidates: func() ([]specCandidate, []string) {
			var list []specdispatch.Spec
			for _, s := range f.specs {
				if ok, _ := specdispatch.Eligible(s, 2); ok {
					list = append(list, s)
				}
			}
			specdispatch.Order(list)
			out := make([]specCandidate, 0, len(list))
			for _, s := range list {
				out = append(out, specCandidate{Spec: s, Rig: "gastown"})
			}
			return out, nil
		},
		Show: func(id string) (specdispatch.Spec, error) {
			s, ok := f.specs[id]
			if !ok {
				return s, errors.New("not found")
			}
			return s, nil
		},
		RigHold: func(rig string) string { return f.rigHold[rig] },
		Roster:  func() (specRoster, error) { return f.roster, f.rosterErr },
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
		Sling: func(c specCandidate, seat specdispatch.SeatChoice) (string, error) {
			if errs := f.slingErrs[c.Spec.ID]; len(errs) > 0 {
				f.slingErrs[c.Spec.ID] = errs[1:]
				if errs[0] != nil {
					return "", errs[0]
				}
			}
			f.slung = append(f.slung, c.Spec.ID)
			f.slingSeats = append(f.slingSeats, seat)
			s := f.specs[c.Spec.ID]
			s.Status, s.Assignee = "hooked", "gastown/polecats/p"
			f.specs[c.Spec.ID] = s
			return "p", nil
		},
		Sleep:    func(time.Duration) { f.sleeps++ },
		Now:      func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
		Template: specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
		Budget: specdispatch.Budget{Seats: []specdispatch.Seat{
			{Agent: "deepseek-flash", Cap: 2},
			{Agent: "claude-sonnet", Cap: 2},
		}},
		PerTick: 1,
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

// The shape gate is the operator's (polecat_pool.shape_gate, gt-cq5gb), and
// the dispatcher carries over every value the seat-refill plugin honored: warn
// slings a badly shaped bead anyway and leaves the verdict on it, refuse skips
// it and labels it needs-shape, off runs no lint at all.
func TestSpecDispatchShapeGate(t *testing.T) {
	t.Parallel()
	shapeless := func() specdispatch.Spec {
		s := cleanSpec("gt-bad", 1, "2026-09-29T10:00:00Z")
		s.Description = strings.Replace(s.Description, "## Gate\nmake gate", "", 1)
		return s
	}
	const refusedLine = "gt-bad: spec lint refused: ## Gate: section missing"

	t.Run("warn slings and notes", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(shapeless())
		env := f.env()
		env.ShapeGate = specdispatch.ShapeGateWarn
		r := runSpecDispatchCycle(env)
		if len(f.slung) != 1 || len(r.Refused) != 0 {
			t.Fatalf("slung %v refused %+v", f.slung, r.Refused)
		}
		if len(f.labels["gt-bad"]) != 0 {
			t.Errorf("warn must not label the bead: %v", f.labels["gt-bad"])
		}
		if n := f.notes["gt-bad"]; len(n) != 1 || !strings.HasPrefix(n[0], "SHAPE: ## Gate: section missing") {
			t.Errorf("notes = %v, want one SHAPE note", n)
		}
		if !strings.Contains(r.Dispatched[0].Line, "SHAPE: ## Gate: section missing") {
			t.Errorf("dispatch line %q must carry the verdict", r.Dispatched[0].Line)
		}
		// A second tick with the same verdict writes no second note.
		runSpecDispatchCycle(env)
		if n := f.notes["gt-bad"]; len(n) != 1 {
			t.Errorf("verdict noted %d times, want once", len(n))
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

func TestSpecSlingParams(t *testing.T) {
	t.Parallel()
	c := specCandidate{Spec: cleanSpec("gt-a", 1, ""), Rig: "gastown"}
	p := specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", c, specdispatch.SeatChoice{Agent: "claude-sonnet"})
	if p.Agent != "claude-sonnet" || !p.NoConvoy || !p.NoBoot || !p.FormulaFailFatal ||
		p.RigName != "gastown" || p.FormulaName != "mol-polecat-work" || !strings.Contains(p.Args, "temporary INSTALL_DIR") {
		t.Fatalf("params = %+v", p)
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
	for i, label := range []string{"gt:ready-to-land", "needs-human", "needs-mayor-review"} {
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
			var out bytes.Buffer
			err := specLint(&out, beadID, spec, showErr, tmpl, false)
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
			var out bytes.Buffer
			err := specLint(&out, beadID, spec, showErr, tmpl, true)
			code, _ := IsSilentExit(err)
			if code != tc.wantExit {
				t.Fatalf("exit code = %d, want %d (%v)", code, tc.wantExit, err)
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
// (gt:ready-to-land, needs-human, deferred, assigned), the non-work types and
// the operator's ceiling are not (gt-4k3fj.8.8 acceptance 1 and 2). The board
// comes from a fake store, so this pins the filter rather than bd's own.
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
			{ID: "gt-human", Type: "task", Status: "open", Priority: 1, Labels: []string{"needs-human"}},
			{ID: "gt-deferred", Type: "task", Status: "deferred", Priority: 1},
			{ID: "gt-epic", Type: "epic", Status: "open", Priority: 1},
			{ID: "gt-p4", Type: "task", Status: "open", Priority: 4},
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

	got, errs := specCandidates(townRoot, 2, fakeBoard)
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	if ids := idList(got); ids != "gt-bug gt-task" {
		t.Errorf("candidates at the default ceiling = %q, want the P1 bug then the P2 task", ids)
	}

	// The ceiling is the operator's (polecat_pool.max_priority): at P4 the
	// feature and the P4 task are the dispatcher's work too.
	got, _ = specCandidates(townRoot, 4, fakeBoard)
	if ids := idList(got); ids != "gt-bug gt-task gt-feature gt-p4" {
		t.Errorf("candidates at a P4 ceiling = %q", ids)
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
	for _, want := range []string{"ready --json", "--unassigned", "--limit 0", "needs-human", "gt:ready-to-land", "spec-dispatch-failed", "--exclude-type", "epic,wisp", "gt:agent"} {
		if !strings.Contains(args, want) {
			t.Errorf("ready args %q missing %q", args, want)
		}
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
