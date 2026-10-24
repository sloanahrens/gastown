package daemon

import (
	"context"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

var specAuditEntry = ScheduledSlingEntry{
	Name: "deep-review", Rig: "gastown", Formula: "mol-deep-review", IntervalStr: "168h", Dispatch: "spec",
	Vars: map[string]string{"scope": "internal/daemon", "depth": "full", "base": "main"},
}

// specSpecOf reads a created run bead the way the dispatcher does.
func specSpecOf(run scheduledRunBead) specdispatch.Spec {
	return specdispatch.Spec{Title: run.Title, Labels: run.Labels, Description: run.Description, Acceptance: run.Acceptance, Priority: run.Priority}
}

func TestScheduledSlingEntry_ValidateDispatch(t *testing.T) {
	t.Parallel()
	if err := scheduledSlingValidate(specAuditEntry); err != nil {
		t.Fatalf("valid spec entry rejected: %v", err)
	}
	base := ScheduledSlingEntry{Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "1h"}
	cases := map[string]struct {
		mutate func(*ScheduledSlingEntry)
		want   string
	}{
		"unknown dispatch": {func(e *ScheduledSlingEntry) { e.Dispatch = "sling" }, `dispatch "sling"`},
		"var with newline": {func(e *ScheduledSlingEntry) { e.Dispatch, e.Vars = "spec", map[string]string{"k": "a\nb=c"} }, "key=value line"},
		"var key with =":   {func(e *ScheduledSlingEntry) { e.Dispatch, e.Vars = "spec", map[string]string{"a=b": "c"} }, "key=value line"},
		"comment var key":  {func(e *ScheduledSlingEntry) { e.Dispatch, e.Vars = "spec", map[string]string{"#a": "c"} }, "key=value line"},
	}
	for name, c := range cases {
		e := base
		c.mutate(&e)
		err := scheduledSlingValidate(e)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, c.want)
		}
	}
	// A spec entry that still names an agent stays valid: it is ignored, not refused.
	for _, agent := range []string{"claude-sonnet", "Claude Sonnet"} {
		stale := base
		stale.Dispatch, stale.Agent = "spec", agent
		if err := scheduledSlingValidate(stale); err != nil {
			t.Errorf("a spec entry naming agent %q must validate: %v", agent, err)
		}
	}
	// An agent with no dispatch, and any vars with no dispatch, stay valid.
	slung := base
	slung.Agent, slung.Vars = "deepseek-pro", map[string]string{"k": "a\nb"}
	if err := scheduledSlingValidate(slung); err != nil {
		t.Errorf("an entry without dispatch must validate as before: %v", err)
	}
}

func TestRunScheduledSlingEntry_SpecDispatchCreatesOneLabelledBeadAndNeverSlings(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	if err := d.runScheduledSlingEntry(context.Background(), specAuditEntry, now); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 1 || f.runs[0].Title != "deep-review 2026-10-14" {
		t.Fatalf("created = %+v", f.runs)
	}
	if len(f.slungIDs) != 0 || len(f.closedIDs) != 0 {
		t.Errorf("spec dispatch must not sling or close: slung %v closed %v", f.slungIDs, f.closedIDs)
	}
	run := f.runs[0]
	if want := []string{"scheduled:deep-review", "formula:mol-deep-review"}; !slices.Equal(run.Labels, want) {
		t.Errorf("labels = %v, want %v", run.Labels, want)
	}
	if want := config.DefaultSeatRefillMaxPriority; run.Priority != want {
		t.Errorf("priority = %d, want the dispatcher ceiling %d (the entry sets none)", run.Priority, want)
	}
	if run.Acceptance == "" {
		t.Error("the acceptance field must be set")
	}
}

// specRunOf is the bead createSpecDispatchedRun makes, as the dispatcher's
// candidate filter sees it once the board hands it over open and unassigned.
func specRunOf(t *testing.T, d *Daemon, f *fakeScheduledRunner, e ScheduledSlingEntry) specdispatch.Spec {
	t.Helper()
	if err := d.runScheduledSlingEntry(context.Background(), e, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 1 {
		t.Fatalf("created = %+v", f.runs)
	}
	s := specSpecOf(f.runs[0])
	s.ID, s.Type, s.Status = "gt-run1", "task", "open"
	return s
}

// The run the scheduler creates must clear the dispatcher's own filter under
// the default polecat_pool.max_priority; a P3 bead was backlog to it and was
// never slung.
func TestRunScheduledSlingEntry_SpecDispatchRunIsDispatchable(t *testing.T) {
	t.Parallel()
	for name, prio := range map[string]int{"no priority": 0, "above the ceiling": 4, "below the ceiling": 1} {
		e := specAuditEntry
		e.Priority = prio
		f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
		d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
		s := specRunOf(t, d, f, e)
		if ok, why := specdispatch.Eligible(s, config.DefaultSeatRefillMaxPriority); !ok {
			t.Errorf("%s: priority %d not dispatchable under the default config: %s", name, s.Priority, why)
		}
	}
}

// A configured ceiling is the bound, not the built-in P2.
func TestRunScheduledSlingEntry_SpecDispatchFollowsConfiguredCeiling(t *testing.T) {
	t.Parallel()
	for _, ceiling := range []int{0, 1, 3} {
		f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
		d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
		ts := config.NewTownSettings()
		ts.PolecatPool = &config.PolecatPool{MaxPriority: &ceiling}
		if err := os.MkdirAll(filepath.Join(d.config.TownRoot, "settings"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := config.SaveTownSettings(config.TownSettingsPath(d.config.TownRoot), ts); err != nil {
			t.Fatal(err)
		}
		s := specRunOf(t, d, f, specAuditEntry)
		if want := min(defaultScheduledSlingPriority, ceiling); s.Priority != want {
			t.Errorf("ceiling %d: priority = %d, want %d", ceiling, s.Priority, want)
		}
		if ok, why := specdispatch.Eligible(s, ceiling); !ok {
			t.Errorf("ceiling %d: not dispatchable: %s", ceiling, why)
		}
	}
}

// A spec run the dispatcher never takes is a failed tick, so the third one
// escalates; a run still inside its bound, or already hooked, is not.
func TestRunScheduledSlingEntry_SpecDispatchUntakenRunEscalates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-untakenSpecBound(specAuditEntry) - time.Hour)

	f := &fakeScheduledRunner{beads: []scheduledBead{{ID: "gt-run1", Status: "open", CreatedAt: old}}}
	d, escalations := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
	for range scheduledSlingEscalateAfter + 1 {
		d.runScheduledSlings()
	}
	if len(*escalations) != 1 || !strings.Contains((*escalations)[0], "gt-run1") {
		t.Errorf("escalations = %v, want one naming gt-run1", *escalations)
	}
	if len(f.runs) != 0 || len(f.closedIDs) != 0 {
		t.Errorf("an untaken spec run must be neither replaced nor closed: created %v closed %v", f.runs, f.closedIDs)
	}

	for name, b := range map[string]scheduledBead{
		"queued inside the bound": {ID: "gt-run2", Status: "open", CreatedAt: now.Add(-time.Hour)},
		"hooked past the bound":   {ID: "gt-run3", Status: "hooked", CreatedAt: old},
		"in progress":             {ID: "gt-run4", Status: "in_progress", CreatedAt: old},
	} {
		f := &fakeScheduledRunner{beads: []scheduledBead{b}}
		d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
		if err := d.runScheduledSlingEntry(context.Background(), specAuditEntry, now); err != nil {
			t.Errorf("%s: err = %v, want skip-open", name, err)
		}
	}
}

// A spec run reopened by dead-holder recovery keeps its original created_at, so
// its age says nothing about how long the dispatcher has had it: it raises no
// untaken-run escalation however old it is (gt-84o0a).
func TestReportUntakenSpecRuns_SkipsAReopenedRun(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-untakenSpecBound(specAuditEntry) - time.Hour)
	for _, tc := range []struct {
		name    string
		runs    []scheduledBead
		wantIDs string // "" means no error
	}{
		{"never taken", []scheduledBead{{ID: "gt-a", Status: "open", CreatedAt: old}}, "gt-a"},
		{"reopened", []scheduledBead{{ID: "gt-b", Status: "open", CreatedAt: old, ResumeBranch: "polecat/nux/gt-b+x"}}, ""},
		{"reopened beside a never-taken run", []scheduledBead{
			{ID: "gt-b", Status: "open", CreatedAt: old, ResumeBranch: "polecat/nux/gt-b+x"},
			{ID: "gt-a", Status: "open", CreatedAt: old},
		}, "gt-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reportUntakenSpecRuns(tc.runs, specAuditEntry, now)
			switch {
			case tc.wantIDs == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tc.wantIDs != "" && (err == nil || !strings.Contains(err.Error(), tc.wantIDs) || strings.Contains(err.Error(), "gt-b")):
				t.Fatalf("err = %v, want one naming only %s", err, tc.wantIDs)
			}
		})
	}
}

func TestRunScheduledSlingEntry_SpecDispatchUsesEntryPriority(t *testing.T) {
	t.Parallel()
	e := specAuditEntry
	e.Priority = 1
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	if err := d.runScheduledSlingEntry(context.Background(), e, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 1 || f.runs[0].Priority != 1 {
		t.Errorf("created = %+v, want priority 1", f.runs)
	}
}

// The generated body is what the dispatcher lints and reads: the real lint
// clears it, and the dispatcher recovers the formula, the sorted vars and the
// scheduled actor from it.
func TestScheduledSpecBody_PassesLintAndDeclaresTheRun(t *testing.T) {
	t.Parallel()
	for name, e := range map[string]ScheduledSlingEntry{
		"with vars": specAuditEntry,
		"no vars":   {Name: "deep-review", Rig: "gastown", Formula: "mol-deep-review", IntervalStr: "1h", Dispatch: "spec"},
	} {
		f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
		d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
		if err := d.runScheduledSlingEntry(context.Background(), e, time.Now()); err != nil {
			t.Fatal(err)
		}
		spec := specSpecOf(f.runs[0])
		if v := specdispatch.Lint(spec, specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"}); !v.Clean() {
			t.Errorf("%s: lint = %s\n%s", name, v.Line("gt-run1"), spec.Description)
		}
		run, err := specdispatch.FormulaRunOf(spec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var wantVars []string
		if len(e.Vars) > 0 {
			wantVars = []string{"base=main", "depth=full", "scope=internal/daemon"}
		}
		if run.Formula != "mol-deep-review" || !slices.Equal(run.Vars, wantVars) || run.Actor != "daemon/scheduled:deep-review" {
			t.Errorf("%s: declared run = %+v, want vars %v", name, run, wantVars)
		}
	}
}

// A second tick finds the first tick's open bead: the queue wait never piles
// up beads, and the wait is not a failure.
func TestRunScheduledSlingEntry_SpecDispatchSecondTickSkipsOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, escalations := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	d.runScheduledSlings()
	if len(f.runs) != 1 {
		t.Fatalf("first tick created %d beads, want 1", len(f.runs))
	}
	d.runScheduledSlings()
	d.runScheduledSlings()
	if len(f.runs) != 1 || len(f.slungIDs) != 0 {
		t.Errorf("later ticks must not create or sling: runs %d slung %v", len(f.runs), f.slungIDs)
	}
	if got := strings.Count(logs.String(), "deep-review: skip-open"); got != 2 {
		t.Errorf("skip-open logged %d times, want 2; log:\n%s", got, logs.String())
	}
	if d.scheduledSlingFailures["deep-review"] != 0 || len(*escalations) != 0 {
		t.Errorf("a queued run is no failure: counter %d, escalations %v", d.scheduledSlingFailures["deep-review"], *escalations)
	}
}

// A run the dispatcher has queued for longer than a sling budget is waiting,
// not stalled: only a scheduler-slung run bead is retired for staleness.
func TestRunScheduledSlingEntry_SpecDispatchQueuedRunIsNeverRetiredAsStalled(t *testing.T) {
	t.Parallel()
	old := scheduledBead{ID: "gt-queued", Status: "open", CreatedAt: time.Now().Add(-6 * time.Hour)}

	f := &fakeScheduledRunner{beads: []scheduledBead{old}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), specAuditEntry, time.Now()); err != nil {
		t.Fatalf("a queued spec run must not be an error: %v", err)
	}
	if len(f.closedIDs) != 0 || len(f.runs) != 0 {
		t.Errorf("queued run touched: closed %v created %d", f.closedIDs, len(f.runs))
	}

	// The unchanged default: the same old bead on a slung entry is retired.
	g := &fakeScheduledRunner{beads: []scheduledBead{old}}
	d, _ = newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, g)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err == nil {
		t.Error("an entry without dispatch must still retire a stalled run bead")
	}
	if len(g.closedIDs) != 1 {
		t.Errorf("stalled run closed = %v", g.closedIDs)
	}
}

// Entries without dispatch behave exactly as before: the scheduler's own
// label only, no acceptance field, and the sling.
func TestRunScheduledSlingEntry_NoDispatchKeepsTheSlingPath(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 1 || !slices.Equal(f.runs[0].Labels, []string{"scheduled:doc-audit"}) || f.runs[0].Acceptance != "" {
		t.Errorf("created = %+v", f.runs)
	}
	if len(f.slungIDs) != 1 {
		t.Errorf("slung = %v, want one sling", f.slungIDs)
	}
}

// A failed create is a failed tick, as for a slung entry.
func TestRunScheduledSlingEntry_SpecDispatchCreateFailureCounts(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createErr: context.DeadlineExceeded}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
	d.runScheduledSlings()
	if d.scheduledSlingFailures["deep-review"] != 1 || len(f.slungIDs) != 0 {
		t.Errorf("failures %d slung %v", d.scheduledSlingFailures["deep-review"], f.slungIDs)
	}
}

// The real runner writes the labels and the acceptance field onto the bead.
func TestExecScheduledSlingRunner_CreatesSpecRunBead(t *testing.T) {
	t.Parallel()
	rig := beadsfake.New()
	r := &execScheduledSlingRunner{townRoot: t.TempDir(), gtPath: "gt", open: func(string) beads.Client { return rig }}
	desc, acc := scheduledSpecBody(specAuditEntry)
	id, err := r.createBead(context.Background(), "gastown", scheduledRunBead{
		Title: "t", Labels: []string{"scheduled:deep-review", "formula:mol-deep-review"}, Description: desc, Acceptance: acc, Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	is, err := rig.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	if is.AcceptanceCriteria != acc || is.Priority != 2 || !slices.Equal(slices.Sorted(slices.Values(is.Labels)), []string{"formula:mol-deep-review", "scheduled:deep-review"}) {
		t.Errorf("created run bead = %+v", is)
	}
}

// A spec-dispatched run takes a seat like any bead: its labels carry no agent:
// pin, so the dispatcher may put it on any free seat.
func TestRunScheduledSlingEntry_SpecDispatchWritesNoAgentPin(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)
	spec := specRunOf(t, d, f, specAuditEntry)
	if want := []string{"scheduled:deep-review", "formula:mol-deep-review"}; !slices.Equal(f.runs[0].Labels, want) {
		t.Errorf("labels = %v, want %v", f.runs[0].Labels, want)
	}
	if got := spec.PinnedAgents(); len(got) != 0 {
		t.Errorf("a spec run must not pin: %v", got)
	}
	// Whichever seat is the only free one takes the run.
	for _, free := range []string{"deepseek-flash", "claude-sonnet"} {
		seats := []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2, Live: 2}, {Agent: "claude-sonnet", Cap: 2, Live: 2}}
		for i := range seats {
			if seats[i].Agent == free {
				seats[i].Live = 0
			}
		}
		b := specdispatch.Budget{Seats: seats, Now: time.Now()}
		if got := specdispatch.ChooseSeat(b, spec); got.Skip || got.Agent != free {
			t.Errorf("the scheduled run took %+v, want the one free seat %s", got, free)
		}
	}
	// With both seats free the draw reaches both, so no model is excluded.
	b := specdispatch.Budget{
		Seats: []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}, {Agent: "claude-sonnet", Cap: 2}},
		Now:   time.Now(),
		Rand:  rand.New(rand.NewSource(1)),
	}
	drawn := map[string]bool{}
	for range 50 {
		got := specdispatch.ChooseSeat(b, spec)
		if got.Skip {
			t.Fatalf("the scheduled run was skipped with free seats: %+v", got)
		}
		drawn[got.Agent] = true
	}
	if len(drawn) != 2 {
		t.Errorf("the draw reached %v, want both seats", drawn)
	}
}

// An entry written before the seat map may still name an agent. It must not be
// skipped as invalid (that silently stops the schedule): the run is created
// with no agent: pin and the log says the agent is ignored.
func TestRunScheduledSlingEntry_SpecDispatchIgnoresAStaleAgent(t *testing.T) {
	t.Parallel()
	e := specAuditEntry
	e.Agent = "claude-sonnet"
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	d.runScheduledSlings()
	if len(f.runs) != 1 {
		t.Fatalf("created = %+v, want one run for an entry that names an agent; log:\n%s", f.runs, logs.String())
	}
	if want := []string{"scheduled:deep-review", "formula:mol-deep-review"}; !slices.Equal(f.runs[0].Labels, want) {
		t.Errorf("labels = %v, want %v", f.runs[0].Labels, want)
	}
	if !strings.Contains(logs.String(), `ignoring agent "claude-sonnet"`) {
		t.Errorf("log does not say the agent is ignored:\n%s", logs.String())
	}
}

// run_agent is the agent: label the scheduler puts on each run bead it creates,
// which is how a scheduled spec entry keeps its runs off a model (deep reviews
// never on DeepSeek, gt-tplg6). An entry without it makes the bead it always
// did, and run_agent is refused on an entry that is not spec-dispatched or that
// names more than one agent.
func TestScheduledSpecRunAgentLabelsTheRunBead(t *testing.T) {
	t.Parallel()
	pinned := specAuditEntry
	pinned.RunAgent = "claude-sonnet"
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{pinned}, f)
	if err := d.runScheduledSlingEntry(context.Background(), pinned, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 1 {
		t.Fatalf("created = %+v", f.runs)
	}
	if want := []string{"scheduled:deep-review", "formula:mol-deep-review", "agent:claude-sonnet"}; !slices.Equal(f.runs[0].Labels, want) {
		t.Errorf("labels = %v, want %v", f.runs[0].Labels, want)
	}
	if got := scheduledSpecLabels(specAuditEntry); slices.Contains(got, "agent:claude-sonnet") || len(got) != 2 {
		t.Errorf("an entry without run_agent got labels %v", got)
	}
	// The dispatcher reads that label back as a pin to the agent's seats.
	run := specdispatch.Spec{Labels: f.runs[0].Labels}
	if got := run.PinnedAgents(); !slices.Equal(got, []string{"claude-sonnet"}) {
		t.Errorf("the dispatcher reads pins %v from the run bead, want [claude-sonnet]", got)
	}

	bad := specAuditEntry
	bad.RunAgent = "claude-sonnet"
	bad.Dispatch = ""
	if err := scheduledSlingValidate(bad); err == nil || !strings.Contains(err.Error(), "run_agent needs dispatch") {
		t.Errorf("run_agent without dispatch spec validated: %v", err)
	}
	bad = specAuditEntry
	bad.RunAgent = "claude-sonnet,claude-opus"
	if err := scheduledSlingValidate(bad); err == nil || !strings.Contains(err.Error(), "one agent name") {
		t.Errorf("a list of agents in run_agent validated: %v", err)
	}
}
