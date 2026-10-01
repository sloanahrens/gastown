package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

func TestScheduledSlingEntry_Validate(t *testing.T) {
	t.Parallel()
	good := ScheduledSlingEntry{Name: "doc-audit", Rig: "gastown", Formula: "mol-doc-audit", IntervalStr: "168h"}
	if err := scheduledSlingValidate(good); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	cases := map[string]ScheduledSlingEntry{
		"empty name":    {Rig: "gastown", Formula: "f", IntervalStr: "1h"},
		"bad name":      {Name: "Doc Audit", Rig: "gastown", Formula: "f", IntervalStr: "1h"},
		"empty rig":     {Name: "a", Formula: "f", IntervalStr: "1h"},
		"empty formula": {Name: "a", Rig: "gastown", IntervalStr: "1h"},
		"no interval":   {Name: "a", Rig: "gastown", Formula: "f"},
		"bad interval":  {Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "weekly"},
		"zero interval": {Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "0s"},
	}
	for name, e := range cases {
		if err := scheduledSlingValidate(e); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := scheduledSlingLabel(good); got != "scheduled:doc-audit" {
		t.Errorf("label = %q", got)
	}
	if got := scheduledSlingPriority(good); got != 3 {
		t.Errorf("default priority = %d, want 3", got)
	}
	if got := scheduledSlingInterval(good); got != 168*time.Hour {
		t.Errorf("interval = %v", got)
	}
}

func TestDecideScheduledSling(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	week := 168 * time.Hour
	cases := []struct {
		name  string
		beads []scheduledBead
		want  scheduledAction
	}{
		{"no beads", nil, scheduledDispatch},
		{"open bead", []scheduledBead{{ID: "gt-1", Status: "open", CreatedAt: now.Add(-30 * 24 * time.Hour)}}, scheduledSkipOpen},
		{"in_progress bead", []scheduledBead{{ID: "gt-1", Status: "in_progress", CreatedAt: now.Add(-2 * time.Hour)}}, scheduledSkipOpen},
		{"closed recent", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-2 * 24 * time.Hour)}}, scheduledSkipRecent},
		{"closed old", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)}}, scheduledDispatch},
		{"mixed: newest closed old, older open", []scheduledBead{
			{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)},
			{ID: "gt-0", Status: "open", CreatedAt: now.Add(-20 * 24 * time.Hour)},
		}, scheduledSkipOpen},
		{"exactly one interval ago", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-week)}}, scheduledDispatch},
	}
	for _, c := range cases {
		if got := decideScheduledSling(c.beads, week, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestIsPatrolEnabled_ScheduledSlingsIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "scheduled_slings") {
		t.Error("nil config must not enable scheduled_slings")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "scheduled_slings") {
		t.Error("absent block must not enable scheduled_slings")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ScheduledSlings: &ScheduledSlingsConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "scheduled_slings") {
		t.Error("enabled block must enable scheduled_slings")
	}
}

func TestParseScheduledBeads_FractionalSecondTimestamp(t *testing.T) {
	t.Parallel()
	// bd/Dolt can emit RFC3339Nano, which encoding/json's strict RFC3339
	// time.Time unmarshaller rejects — one such row used to fail the whole list
	// and feed the spurious-escalation path.
	got := scheduledBeadsOf([]*beads.Issue{{ID: "gt-nano", Status: "closed", CreatedAt: "2026-09-12T10:00:00.123456789Z"}})
	want := time.Date(2026, 9, 12, 10, 0, 0, 123456789, time.UTC)
	if len(got) != 1 || !got[0].CreatedAt.Equal(want) {
		t.Fatalf("parsed %+v, want created_at %v", got, want)
	}
}

func TestDecideScheduledSling_IgnoresRunsThatFailedToSling(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	week := 168 * time.Hour
	failed := scheduledBead{ID: "gt-bad", Status: "closed", CloseReason: scheduledSlingFailureReason, CreatedAt: now.Add(-time.Minute)}
	if got := decideScheduledSling([]scheduledBead{failed}, week, now); got != scheduledDispatch {
		t.Errorf("a run whose sling failed must not hold the interval: got %v", got)
	}
	// The match is by prefix so a bd-normalized reason still counts.
	normalized := failed
	normalized.CloseReason = scheduledSlingFailureMarker + " (normalized by bd)"
	if got := decideScheduledSling([]scheduledBead{normalized}, week, now); got != scheduledDispatch {
		t.Errorf("prefix-matched failure reason must be ignored: got %v", got)
	}
	// A run that actually completed still gates the interval.
	done := scheduledBead{ID: "gt-ok", Status: "closed", CloseReason: "audit complete", CreatedAt: now.Add(-2 * time.Hour)}
	if got := decideScheduledSling([]scheduledBead{done}, week, now); got != scheduledSkipRecent {
		t.Errorf("a completed run must still hold the interval: got %v", got)
	}
}

// --- Task 8: dispatch runner, single flight, escalation ---

// fakeScheduledRunner models production bead state: a created bead is open
// until closeBead retires it, so each tick sees what a real tick would see.
type fakeScheduledRunner struct {
	beads     []scheduledBead
	listErr   error
	created   []string // bead titles, in order
	createID  string
	createErr error
	slungIDs  []string
	slung     []ScheduledSlingEntry
	slingErr  error
	closedIDs []string
	closedWhy []string
	now       time.Time
}

func (f *fakeScheduledRunner) listBeads(_ context.Context, _, _ string) ([]scheduledBead, error) {
	return f.beads, f.listErr
}

func (f *fakeScheduledRunner) createBead(_ context.Context, _, title, _, _ string, _ int) (string, error) {
	f.created = append(f.created, title)
	if f.createErr != nil {
		return "", f.createErr
	}
	f.beads = append(f.beads, scheduledBead{ID: f.createID, Status: "open", CreatedAt: f.now})
	return f.createID, nil
}

func (f *fakeScheduledRunner) sling(_ context.Context, beadID string, e ScheduledSlingEntry) error {
	f.slungIDs = append(f.slungIDs, beadID)
	f.slung = append(f.slung, e)
	return f.slingErr
}

func (f *fakeScheduledRunner) closeBead(_ context.Context, _, beadID, reason string) error {
	f.closedIDs = append(f.closedIDs, beadID)
	f.closedWhy = append(f.closedWhy, reason)
	for i := range f.beads {
		if f.beads[i].ID == beadID {
			f.beads[i].Status = "closed"
			f.beads[i].CloseReason = reason
		}
	}
	return nil
}

func newScheduledTestDaemon(t *testing.T, entries []ScheduledSlingEntry, runner scheduledSlingRunner) (*Daemon, *[]string) {
	t.Helper()
	var escalations []string
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ScheduledSlings: &ScheduledSlingsConfig{Enabled: true, Entries: entries},
		}},
		scheduledSlingRunner:   runner,
		scheduledSlingFailures: map[string]int{},
		scheduledSlingEscalate: func(source, msg string) { escalations = append(escalations, source+": "+msg) },
	}
	return d, &escalations
}

var docAuditEntry = ScheduledSlingEntry{Name: "doc-audit", Rig: "gastown", Formula: "mol-doc-audit", Agent: "deepseek-pro", IntervalStr: "168h"}

func TestRunScheduledSlingEntry_DispatchesWhenDue(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, now); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0] != "doc-audit 2026-09-19" {
		t.Errorf("created titles = %v", f.created)
	}
	if len(f.slungIDs) != 1 || f.slungIDs[0] != "gt-run1" || f.slung[0].Agent != "deepseek-pro" {
		t.Errorf("sling calls = %v %v", f.slungIDs, f.slung)
	}
	if len(f.closedIDs) != 0 {
		t.Errorf("a successful dispatch must leave its bead open, closed %v", f.closedIDs)
	}
}

func TestRunScheduledSlingEntry_SkipsWhileOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{beads: []scheduledBead{{ID: "gt-old", Status: "open", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("expected no dispatch while a run bead is open, got create=%v sling=%v", f.created, f.slungIDs)
	}
}

// A failed sling must retire its bead and stay escalatable. Before the fix the
// bead stayed open, every later tick read it as skip-open, reported success,
// and reset the counter — making the third-failure escalation unreachable for
// exactly the failure it exists to surface.
func TestRunScheduledSlings_EscalatesOnThirdConsecutiveFailure(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New("boom"), now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	for i := 1; i <= 3; i++ {
		d.runScheduledSlings()
		if got := d.scheduledSlingFailures["doc-audit"]; got != i {
			t.Fatalf("after tick %d: consecutive failures = %d, want %d", i, got, i)
		}
	}
	if len(*esc) != 1 {
		t.Fatalf("expected exactly one escalation after three failures, got %d: %v", len(*esc), *esc)
	}
	if len(f.closedIDs) != 3 {
		t.Errorf("each failed run must retire its bead, closed %v", f.closedIDs)
	}
	d.runScheduledSlings()
	if len(*esc) != 1 {
		t.Errorf("fourth failure must not escalate again, got %d", len(*esc))
	}
	f.slingErr = nil
	d.runScheduledSlings() // retired beads are ignored, so this dispatches and succeeds
	if d.scheduledSlingFailures["doc-audit"] != 0 {
		t.Errorf("success must reset the failure count, got %d", d.scheduledSlingFailures["doc-audit"])
	}
}

func TestRunScheduledSlingEntry_ClosesBeadWhenSlingFails(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New("boom"), now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err == nil {
		t.Fatal("a failed sling must be reported so the failure counter sees it")
	}
	if len(f.closedIDs) != 1 || f.closedIDs[0] != "gt-x" {
		t.Fatalf("failed run bead must be retired, closed %v", f.closedIDs)
	}
	if !strings.HasPrefix(f.closedWhy[0], scheduledSlingFailureMarker) {
		t.Errorf("close reason %q must carry the failure marker", f.closedWhy[0])
	}
	if got := decideScheduledSling(f.beads, scheduledSlingInterval(docAuditEntry), time.Now()); got != scheduledDispatch {
		t.Errorf("the next tick after a failed sling = %v, want dispatch", got)
	}
}

func TestRunScheduledSlings_InvalidEntryIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-ok", now: time.Now()}
	bad := ScheduledSlingEntry{Name: "bad", Rig: "gastown", Formula: "f", IntervalStr: "weekly"}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{bad, docAuditEntry}, f)
	d.runScheduledSlings()
	if len(f.slungIDs) != 1 {
		t.Errorf("valid entry must still dispatch, got %v", f.slungIDs)
	}
	if len(*esc) != 0 {
		t.Errorf("a config error is logged, not escalated: %v", *esc)
	}
}

func TestTriggerScheduledSlings_SingleFlight(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: discardLogger} // patrol inactive: run returns at once
	d.scheduledSlingsRunning.Store(true)
	if d.triggerScheduledSlings() {
		t.Error("a trigger must skip while a cycle is running")
	}
	d.scheduledSlingsRunning.Store(false)
	if !d.triggerScheduledSlings() {
		t.Fatal("a trigger with no cycle running should start one")
	}
}

func TestExecScheduledRunner_SlingArgv(t *testing.T) {
	t.Parallel()
	r := &execScheduledSlingRunner{townRoot: "/town", bdPath: "bd", gtPath: "gt"}
	e := docAuditEntry
	e.Vars = map[string]string{"slice_docs": "8"}
	got := r.slingArgs("gt-run1", e)
	want := []string{"sling", "gt-run1", "gastown", "--formula=mol-doc-audit", "--agent=deepseek-pro", "--actor=daemon/scheduled:doc-audit", "--no-boot", "--var", "slice_docs=8"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv\n got %v\nwant %v", got, want)
	}
}

// TestExecScheduledSlingRunner_ListsClosedRunsInTheRigDatabase: the run
// list includes closed runs, read from the rig's own database. Without the
// closed ones the newest completed run is invisible and the interval guard is
// defeated.
func TestExecScheduledSlingRunner_ListsClosedRunsInTheRigDatabase(t *testing.T) {
	t.Parallel()
	label := scheduledSlingLabel(docAuditEntry)
	rig := beadsfake.New()
	rig.Seed(
		beads.Issue{ID: "gt-done", Status: "closed", CloseReason: "audit complete", CreatedAt: "2026-09-12T10:00:00.5Z", Labels: []string{label}},
		beads.Issue{ID: "gt-open", CreatedAt: "2026-09-11T10:00:00Z", Labels: []string{label}},
		beads.Issue{ID: "gt-other", Labels: []string{"scheduled:other"}},
	)
	var opened []string
	r := &execScheduledSlingRunner{townRoot: t.TempDir(), gtPath: "gt", open: func(name string) beads.Client {
		opened = append(opened, name)
		return rig
	}}

	got, err := r.listBeads(context.Background(), "gastown", label)
	if err != nil {
		t.Fatalf("listBeads: %v", err)
	}
	byID := map[string]scheduledBead{}
	for _, b := range got {
		byID[b.ID] = b
	}
	if len(got) != 2 || byID["gt-open"].Status != "open" {
		t.Fatalf("listBeads = %+v, want the open and the closed run only", got)
	}
	done := byID["gt-done"]
	if done.Status != "closed" || done.CloseReason != "audit complete" || !done.CreatedAt.Equal(time.Date(2026, 9, 12, 10, 0, 0, 500000000, time.UTC)) {
		t.Errorf("closed run = %+v", done)
	}
	if strings.Join(opened, ",") != "gastown" {
		t.Errorf("databases opened = %q, want the gastown rig's", opened)
	}
}

// TestExecScheduledSlingRunner_CreatesAndClosesRunBeads: a run bead is
// created labeled, at the entry's priority and with its description, and a
// failed run is closed with its reason.
func TestExecScheduledSlingRunner_CreatesAndClosesRunBeads(t *testing.T) {
	t.Parallel()
	rig := beadsfake.New()
	r := &execScheduledSlingRunner{townRoot: t.TempDir(), gtPath: "gt", open: func(string) beads.Client { return rig }}
	ctx := context.Background()

	id, err := r.createBead(ctx, "gastown", "doc audit", "scheduled:doc-audit", "run body", 3)
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if err := r.closeBead(ctx, "gastown", id, scheduledSlingFailureMarker+"boom"); err != nil {
		t.Fatalf("closeBead: %v", err)
	}
	is, err := rig.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != "doc audit" || is.Priority != 3 || is.Description != "run body" || strings.Join(is.Labels, ",") != "scheduled:doc-audit" {
		t.Errorf("created run bead = %+v", is)
	}
	if is.Status != "closed" || is.CloseReason != scheduledSlingFailureMarker+"boom" {
		t.Errorf("closed run bead: status %q reason %q", is.Status, is.CloseReason)
	}
}

