package daemon

import (
	"context"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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
		"spec with agent":  {func(e *ScheduledSlingEntry) { e.Dispatch, e.Agent = "spec", "deepseek-pro" }, "must be empty"},
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
	if run.Priority != defaultScheduledSlingPriority {
		t.Errorf("priority = %d, want the default %d", run.Priority, defaultScheduledSlingPriority)
	}
	if run.Acceptance == "" {
		t.Error("the acceptance field must be set")
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
