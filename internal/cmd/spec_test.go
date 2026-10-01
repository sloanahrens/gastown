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

func cleanSpec(id string, priority int, created string) specdispatch.Spec {
	return specdispatch.Spec{
		ID: id, Title: "Add " + id, Type: "feature", Status: "open", Priority: priority, CreatedAt: created,
		Labels: []string{"spec"}, Description: specTestDescription, Acceptance: "- [ ] a\n- [ ] b\n- [ ] c",
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
				if ok, _ := specdispatch.Eligible(s); ok {
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
			{Agent: "deepseek-flash", Class: specdispatch.ClassHooked, Cap: 2},
			{Agent: "local-coder", Class: specdispatch.ClassHookless, Cap: 2},
			{Agent: "claude-sonnet", Class: specdispatch.ClassHooked, Cap: 2},
		}},
		PerTick: 1,
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
	// The first seat until its cap (2), then the next hooked seat; the
	// hookless seat in between is skipped for specs without host-safe.
	if f.slingSeats[0].Agent != "deepseek-flash" || f.slingSeats[1].Agent != "deepseek-flash" || f.slingSeats[2].Agent != "claude-sonnet" {
		t.Errorf("seats = %+v", f.slingSeats)
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

func TestSpecDispatchHostSafety(t *testing.T) {
	t.Parallel()
	full := map[string]int{"deepseek-flash": 2, "claude-sonnet": 2}

	// No host-safe label: hooked seats only, so a free hookless seat is not
	// taken and the spec waits without an annotation.
	plain := cleanSpec("gt-plain", 1, "2026-09-29T10:00:00Z")
	f := newFakeSpecTown(plain)
	f.roster = specRoster{Live: full}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "hooked seats only") || len(f.notes) != 0 {
		t.Fatalf("slung %v skipped %+v notes %v", f.slung, r.Skipped, f.notes)
	}

	// "installer docs" is not an install: dispatched on a hooked seat, no note.
	docs := cleanSpec("gt-docs", 1, "2026-09-29T10:00:00Z")
	docs.Title = "Write the installer docs"
	f = newFakeSpecTown(docs)
	runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slingSeats[0].Class != specdispatch.ClassHooked || len(f.notes) != 0 {
		t.Fatalf("installer docs: slung %v seats %+v notes %v", f.slung, f.slingSeats, f.notes)
	}

	// host-safe label and no risky terms: the hookless seat is allowed.
	safe := cleanSpec("gt-safe", 1, "2026-09-29T10:00:00Z")
	safe.Labels = append(safe.Labels, specdispatch.HostSafeLabel)
	f = newFakeSpecTown(safe)
	f.roster = specRoster{Live: map[string]int{"deepseek-flash": 2}}
	runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slingSeats[0].Agent != "local-coder" || f.slingSeats[0].Class != specdispatch.ClassHookless {
		t.Fatalf("host-safe: seats %+v", f.slingSeats)
	}

	// host-safe label but "rm -fr" in the text: forced hooked, annotated once.
	risky := cleanSpec("gt-risky", 1, "2026-09-29T10:00:00Z")
	risky.Labels = append(risky.Labels, specdispatch.HostSafeLabel, "route:flash")
	risky.Description += "\n\nClean up with rm -fr build/ first."
	f = newFakeSpecTown(risky)
	f.roster = specRoster{Live: map[string]int{"deepseek-flash": 2}}
	runSpecDispatchCycle(f.env())
	if len(f.slung) != 1 || f.slingSeats[0].Agent != "claude-sonnet" || !f.slingSeats[0].OverrodeHostSafe {
		t.Fatalf("risky host-safe: seats %+v", f.slingSeats)
	}
	if n := len(f.notes["gt-risky"]); n != 1 || !strings.Contains(f.notes["gt-risky"][0], "host-safe label overridden") {
		t.Fatalf("override notes = %v", f.notes["gt-risky"])
	}

	// Hooked seats full: the forced spec waits, never taking the hookless
	// seat, and the override is still noted only once across ticks.
	f = newFakeSpecTown(risky)
	f.roster = specRoster{Live: full}
	runSpecDispatchCycle(f.env())
	r = runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || !strings.Contains(r.Skipped[0].Line, "hooked seats only") || len(f.notes["gt-risky"]) != 1 {
		t.Fatalf("slung %v skipped %+v notes %v", f.slung, r.Skipped, f.notes)
	}
}

func TestSpecSlingParams(t *testing.T) {
	t.Parallel()
	c := specCandidate{Spec: cleanSpec("gt-a", 1, ""), Rig: "gastown"}
	p := specSlingParams("/town", "/town/gastown/.beads", "mol-polecat-work", c, specdispatch.SeatChoice{Agent: "claude-sonnet"})
	if p.Agent != "claude-sonnet" || !p.AgentBeatsRoute || !p.NoConvoy || !p.NoBoot || !p.FormulaFailFatal ||
		p.RigName != "gastown" || p.FormulaName != "mol-polecat-work" || !strings.Contains(p.Args, "temporary INSTALL_DIR") {
		t.Fatalf("params = %+v", p)
	}
}

func TestSpecDispatchRespectsCapsAndRoster(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.roster = specRoster{Live: map[string]int{"deepseek-flash": 2, "local-coder": 2, "claude-sonnet": 2}}
	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 || !strings.Contains(r.Skipped[0].Line, "all full") {
		t.Fatalf("slung %v skipped %+v", f.slung, r.Skipped)
	}
	if r.Roster != "deepseek-flash 2/2 hooked, local-coder 2/2 hookless, claude-sonnet 2/2 hooked" {
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
	bad.Type = "task"
	bad.Labels = []string{"spec"}
	good := cleanSpec("gt-good", 1, "2026-09-29T11:00:00Z")
	f := newFakeSpecTown(good)
	f.specs["gt-bad"] = bad // not eligible by type; exercised via Show path below
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
	ts.Agents = map[string]*config.RuntimeConfig{
		"claude-sonnet":  {Provider: "claude", Command: "claude"},
		"deepseek-flash": {Provider: "claude", Command: "claude", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic"}},
		"local-coder":    {Provider: "openai", Command: "claude"},
	}
	ts.RoleAgents = map[string]string{"polecat": "deepseek-flash"}
	ts.PolecatPool = &config.PolecatPool{LocalAgent: "local-coder-polecat", OverflowAgent: "deepseek-flash", MaxOverflow: 3, MinSpawnGap: "4m"}

	b := specBudgetFromConfig(ts, nil)
	if got := b.Picture(); got != "deepseek-flash 0/3 hooked, claude-sonnet 0/2 hooked" || b.MinSpawnGap != 4*time.Minute {
		t.Fatalf("defaults = %q gap %v", got, b.MinSpawnGap)
	}
	b = specBudgetFromConfig(ts, &config.SpecDispatchConfig{MaxHooked: 1, HooklessAgent: "local-coder", MaxHookless: 1, PreferHooked: true})
	if got := b.Picture(); got != "claude-sonnet 0/1 hooked, deepseek-flash 0/3 hooked, local-coder 0/1 hookless" {
		t.Fatalf("configured = %q", got)
	}
	b = specBudgetFromConfig(ts, &config.SpecDispatchConfig{MaxHooked: -1})
	if got := b.Picture(); got != "deepseek-flash 0/3 hooked, claude-sonnet 0/0 hooked" {
		t.Fatalf("max_hooked<0 must close the seat: %q", got)
	}
	// A hooked_agent that is not provider=claude is dropped, not obeyed.
	b = specBudgetFromConfig(ts, &config.SpecDispatchConfig{HookedAgent: "local-coder"})
	if got := b.Picture(); got != "deepseek-flash 0/3 hooked" {
		t.Fatalf("non-claude hooked_agent kept: %q", got)
	}
	// max_overflow unset caps the overflow seat at the default, never uncapped.
	ts.PolecatPool.MaxOverflow = 0
	if got := specBudgetFromConfig(ts, nil).Picture(); got != "deepseek-flash 0/2 hooked, claude-sonnet 0/2 hooked" {
		t.Fatalf("uncapped overflow = %q", got)
	}
}

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

func TestResolvePolecatPoolAgentExplicitBeatsRouteLabel(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{LocalAgent: "local-coder-polecat", MaxLocal: 0, OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	router := realPoolRouter(townRoot)
	router.sessions = func() sessionLister {
		return &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}}
	}
	router.lookupBead = func(beadID string) (poolBead, error) {
		return poolBead{ID: beadID, Type: "feature", Labels: []string{"spec", routeFlashLabel}}, nil
	}
	// The default path keeps gt-4lbz: the label outranks the request
	// (peekPolecatPoolAgent's route).
	if a, _, err := router.route("gt-a", "claude-sonnet", false, false); a != "deepseek-flash" || err != nil {
		t.Fatalf("default route: got %q %v", a, err)
	}
	// The dispatcher's explicit agent stands (resolvePolecatPoolAgentExplicit's
	// route).
	if a, r, err := router.route("gt-a", "claude-sonnet", true, true); a != "" || r != "" || err != nil {
		t.Fatalf("explicit route: got %q %q %v, want the request untouched", a, r, err)
	}
	if got := withoutRouteLabels([]string{"spec", "Route:Flash", "route:local", "x"}); strings.Join(got, ",") != "spec,x" {
		t.Errorf("withoutRouteLabels = %v", got)
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
		{"clean", `[{"id":"gt-ok","issue_type":"feature","status":"open","labels":["spec"],"description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`, "gt-ok: spec lint ok", 0},
		{"refused", `[{"id":"gt-no","issue_type":"feature","status":"open","labels":["spec"],"description":"## Goal\nx"}]`, "gt-no: spec lint refused: ## Constraints: section missing", 1},
		{"planning", `[{"id":"gt-pl","issue_type":"feature","status":"open","labels":["spec","needs-planning"],"description":"` + jsonEscape(specTestDescription) + `","acceptance_criteria":"- [ ] a"}]`, "gt-pl: spec needs planning: label needs-planning", 2},
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
			err := specLint(&out, beadID, spec, showErr, tmpl)
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
	for _, want := range []string{"ready --json", "--label spec", "--type feature", "--unassigned", "--limit 0", "needs-human", "gt:ready-to-land", "spec-dispatch-failed"} {
		if !strings.Contains(args, want) {
			t.Errorf("ready args %q missing %q", args, want)
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
