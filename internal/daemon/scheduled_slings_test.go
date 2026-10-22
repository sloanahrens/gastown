package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
	const staleAfter = 20 * time.Minute
	cases := []struct {
		name  string
		beads []scheduledBead
		want  scheduledAction
	}{
		{"no beads", nil, scheduledDispatch},
		{"closed recent", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-2 * 24 * time.Hour)}}, scheduledSkipRecent},
		{"closed old", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)}}, scheduledDispatch},
		{"mixed: newest closed old, older run in flight", []scheduledBead{
			{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)},
			{ID: "gt-0", Status: "hooked", CreatedAt: now.Add(-20 * 24 * time.Hour)},
		}, scheduledSkipOpen},
		{"exactly one interval ago", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-week)}}, scheduledDispatch},
	}
	for _, c := range cases {
		if got := decideScheduledSling(c.beads, week, staleAfter, now).action; got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// Status is the whole signal for staleness: gt sling hooks the bead before it
// spawns the polecat, so an open run bead is one no sling ever started. Past
// the sling budget nothing is left to hook it, and the bead must be retired
// rather than read as a run in flight forever (gt-g16z2).
func TestDecideScheduledSling_StaleOpenBeadIsRecoveredNotSkipped(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	week := 168 * time.Hour
	const staleAfter = 20 * time.Minute
	bead := func(status string, age time.Duration) scheduledBead {
		return scheduledBead{ID: "gt-run", Status: status, CreatedAt: now.Add(-age)}
	}
	cases := []struct {
		name  string
		beads []scheduledBead
		want  scheduledAction
		stale []string
	}{
		{"open past the budget plus a tick", []scheduledBead{bead("open", 30*24*time.Hour)}, scheduledRecoverStale, []string{"gt-run"}},
		{"open exactly at the threshold", []scheduledBead{bead("open", staleAfter)}, scheduledSkipOpen, nil},
		{"open inside the budget", []scheduledBead{bead("open", 10*time.Minute)}, scheduledSkipOpen, nil},
		{"hooked past the budget", []scheduledBead{bead("hooked", 30*24*time.Hour)}, scheduledSkipOpen, nil},
		{"in_progress past the budget", []scheduledBead{bead("in_progress", 30*24*time.Hour)}, scheduledSkipOpen, nil},
		{"open with no readable created_at", []scheduledBead{{ID: "gt-run", Status: "open"}}, scheduledSkipOpen, nil},
		{"two stale open beads", []scheduledBead{
			{ID: "gt-a", Status: "open", CreatedAt: now.Add(-time.Hour)},
			{ID: "gt-b", Status: "open", CreatedAt: now.Add(-2 * time.Hour)},
		}, scheduledRecoverStale, []string{"gt-a", "gt-b"}},
		{"a young open bead keeps both in flight", []scheduledBead{
			bead("open", 30*24*time.Hour),
			{ID: "gt-young", Status: "open", CreatedAt: now.Add(-time.Minute)},
		}, scheduledSkipOpen, nil},
	}
	for _, c := range cases {
		dec := decideScheduledSling(c.beads, week, staleAfter, now)
		if dec.action != c.want {
			t.Errorf("%s: action = %v want %v", c.name, dec.action, c.want)
		}
		var got []string
		for _, b := range dec.stale {
			got = append(got, b.ID)
		}
		if strings.Join(got, ",") != strings.Join(c.stale, ",") {
			t.Errorf("%s: stale beads = %v want %v", c.name, got, c.stale)
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
	const staleAfter = 20 * time.Minute
	failed := scheduledBead{ID: "gt-bad", Status: "closed", CloseReason: scheduledSlingFailureReason, CreatedAt: now.Add(-time.Minute)}
	if got := decideScheduledSling([]scheduledBead{failed}, week, staleAfter, now).action; got != scheduledDispatch {
		t.Errorf("a run whose sling failed must not hold the interval: got %v", got)
	}
	// The match is by prefix so a bd-normalized reason still counts.
	normalized := failed
	normalized.CloseReason = scheduledSlingFailureMarker + " (normalized by bd)"
	if got := decideScheduledSling([]scheduledBead{normalized}, week, staleAfter, now).action; got != scheduledDispatch {
		t.Errorf("prefix-matched failure reason must be ignored: got %v", got)
	}
	// A run that actually completed still gates the interval.
	done := scheduledBead{ID: "gt-ok", Status: "closed", CloseReason: "audit complete", CreatedAt: now.Add(-2 * time.Hour)}
	if got := decideScheduledSling([]scheduledBead{done}, week, staleAfter, now).action; got != scheduledSkipRecent {
		t.Errorf("a completed run must still hold the interval: got %v", got)
	}
}

// --- Task 8: dispatch runner, single flight, escalation ---

// fakeScheduledRunner models production bead state: a created bead is open
// until closeBead retires it, so each tick sees what a real tick would see.
type fakeScheduledRunner struct {
	beads         []scheduledBead
	listErr       error
	created       []string // bead titles, in order
	runs          []scheduledRunBead
	createID      string
	createErr     error
	slungIDs      []string
	slungBranches []string // the branch each sling resumed, "" for a fresh run
	slung         []ScheduledSlingEntry
	slingErr      error
	closeErr      error
	closedIDs     []string
	closedWhy     []string
	now           time.Time

	// gate answers: the n-th runGate call returns gateAnswers[n], the last one
	// repeating. gateDeadlines records the time left on each call's context.
	gateAnswers   []gateAnswer
	gateCalls     []string // entry names, in order
	gateDeadlines []time.Duration

	// slingBudgets records the time left on each sling's context; slingHook
	// replaces slingErr when set, to act as a sling that ran into its deadline.
	slingBudgets []time.Duration
	slingHook    func(ctx context.Context, f *fakeScheduledRunner, beadID string) error

	// goneBranches are the branches absent from origin; every other branch is
	// there. probeErr makes the origin probe fail.
	goneBranches []string
	probeErr     error

	// admitReason is the capacity pre-flight's answer, "" for "the sling has a
	// seat"; admitErr makes the probe unable to answer. admitted records the
	// entries the patrol asked about, in order.
	admitReason string
	admitErr    error
	admitted    []string
}

func (f *fakeScheduledRunner) admit(_ context.Context, e ScheduledSlingEntry) (string, error) {
	f.admitted = append(f.admitted, e.Name)
	return f.admitReason, f.admitErr
}

func (f *fakeScheduledRunner) originHasBranch(_ context.Context, _, branch string) (bool, error) {
	if f.probeErr != nil {
		return false, f.probeErr
	}
	return !slices.Contains(f.goneBranches, branch), nil
}

// setStatus moves a run bead to status, as gt sling hooking it would.
func (f *fakeScheduledRunner) setStatus(id, status string) {
	for i := range f.beads {
		if f.beads[i].ID == id {
			f.beads[i].Status = status
		}
	}
}

// gateAnswer is one scripted gate run. timedOut makes the fake behave as a
// command the deadline killed: it returns the kill's error with the gate
// context already expired, without waiting.
type gateAnswer struct {
	out      string
	err      error
	timedOut bool
}

func (f *fakeScheduledRunner) runGate(ctx context.Context, e ScheduledSlingEntry) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		f.gateDeadlines = append(f.gateDeadlines, time.Until(dl))
	}
	f.gateCalls = append(f.gateCalls, e.Name)
	if len(f.gateAnswers) == 0 {
		return nil, nil
	}
	i := len(f.gateCalls) - 1
	if i >= len(f.gateAnswers) {
		i = len(f.gateAnswers) - 1
	}
	a := f.gateAnswers[i]
	if a.timedOut {
		return []byte(a.out), context.DeadlineExceeded
	}
	return []byte(a.out), a.err
}

func (f *fakeScheduledRunner) listBeads(_ context.Context, _, _ string) ([]scheduledBead, error) {
	return f.beads, f.listErr
}

func (f *fakeScheduledRunner) createBead(_ context.Context, _ string, run scheduledRunBead) (string, error) {
	f.created = append(f.created, run.Title)
	f.runs = append(f.runs, run)
	if f.createErr != nil {
		return "", f.createErr
	}
	f.beads = append(f.beads, scheduledBead{ID: f.createID, Status: "open", CreatedAt: f.now})
	return f.createID, nil
}

func (f *fakeScheduledRunner) sling(ctx context.Context, beadID, branch string, e ScheduledSlingEntry) error {
	f.slungIDs = append(f.slungIDs, beadID)
	f.slungBranches = append(f.slungBranches, branch)
	f.slung = append(f.slung, e)
	if dl, ok := ctx.Deadline(); ok {
		f.slingBudgets = append(f.slingBudgets, time.Until(dl))
	}
	if f.slingHook != nil {
		return f.slingHook(ctx, f, beadID)
	}
	return f.slingErr
}

func (f *fakeScheduledRunner) closeBead(_ context.Context, _, beadID, reason string) error {
	f.closedIDs = append(f.closedIDs, beadID)
	f.closedWhy = append(f.closedWhy, reason)
	if f.closeErr != nil {
		return f.closeErr
	}
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

// A young open bead is a sling still running: the tick leaves both it and the
// entry alone. (An old one is recovered — see the recovery tests below.)
func TestRunScheduledSlingEntry_SkipsWhileOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{beads: []scheduledBead{{ID: "gt-old", Status: "open", CreatedAt: time.Now().Add(-time.Minute)}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("expected no dispatch while a run bead is open, got create=%v sling=%v", f.created, f.slungIDs)
	}
	if len(f.closedIDs) != 0 {
		t.Errorf("a young open bead must be left alone, closed %v", f.closedIDs)
	}
}

// The entry dies of the daemon going away between creating the run bead and
// slinging it: no process is left to take the failure-close branch, so the bead
// stays open, and before this fix every later tick read it as skip-open,
// reported success and reset the counter — the entry stayed dead until an
// operator closed the bead by hand and no escalation ever fired (gt-g16z2).
func TestRunScheduledSlings_RecoversTheRunBeadADeadCycleLeftOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{
		createID: "gt-next",
		now:      time.Now(),
		beads:    []scheduledBead{{ID: "gt-crashed", Status: "open", CreatedAt: time.Now().Add(-2 * time.Hour)}},
	}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)

	d.runScheduledSlings()
	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("the tick must retire the stalled bead before dispatching, created %v slung %v", f.created, f.slungIDs)
	}
	if len(f.closedIDs) != 1 || f.closedIDs[0] != "gt-crashed" {
		t.Fatalf("a run bead left open past the budget must be retired, closed %v", f.closedIDs)
	}
	if !strings.HasPrefix(f.closedWhy[0], scheduledSlingFailureMarker) {
		t.Errorf("close reason %q must carry the failure marker, or the retired bead gates the interval", f.closedWhy[0])
	}
	if got := d.scheduledSlingFailures["doc-audit"]; got != 1 {
		t.Errorf("retiring a stalled bead must count as a failure, got %d", got)
	}

	// The retired bead is ignored, so the next tick runs the entry again.
	d.runScheduledSlings()
	if len(f.slungIDs) != 1 || f.slungIDs[0] != "gt-next" {
		t.Fatalf("the tick after the recovery must dispatch, slung %v", f.slungIDs)
	}
	if got := d.scheduledSlingFailures["doc-audit"]; got != 0 {
		t.Errorf("a successful dispatch resets the count, got %d", got)
	}
	if len(*esc) != 0 {
		t.Errorf("a single recovery is not three failures, escalated %v", *esc)
	}
}

// A stalled bead the patrol cannot retire (the close write fails) leaves the
// entry dead as before, but no longer silently: every tick counts a failure and
// the third escalates. Before this fix a skip reset the count, so exactly this
// case never reached an operator.
func TestRunScheduledSlings_AStalledBeadItCannotRetireStillEscalates(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-next", now: time.Now(), closeErr: errors.New("dolt down")}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	for i := 1; i <= 3; i++ {
		f.beads = []scheduledBead{{ID: "gt-stalled", Status: "open", CreatedAt: time.Now().Add(-time.Hour)}}
		d.runScheduledSlings()
		if got := d.scheduledSlingFailures["doc-audit"]; got != i {
			t.Fatalf("after tick %d: consecutive failures = %d, want %d", i, got, i)
		}
		if len(f.slungIDs) != 0 {
			t.Fatalf("tick %d dispatched beside a bead still open: slung %v", i, f.slungIDs)
		}
	}
	if len(*esc) != 1 {
		t.Fatalf("expected the third stalled tick to escalate, got %d: %v", len(*esc), *esc)
	}
}

// A run that started and lost its polecat is returned to open by the dead-holder
// recovery with its created_at untouched and the surviving branch in its notes.
// It must not read as a run no sling ever hooked: retiring it as stalled would
// dispatch a second run from main and orphan the branch (gt-k6kea).
func TestRunScheduledSlings_AReopenedRunResumesItsBranchInsteadOfBeingRetired(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-next", now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	f.slingHook = func(_ context.Context, f *fakeScheduledRunner, id string) error {
		f.setStatus(id, "hooked") // gt sling hooks the bead before it spawns the polecat
		return nil
	}

	// started: hooked at creation, then reopened by the recovery two hours later.
	created := time.Now().Add(-2 * time.Hour)
	f.beads = []scheduledBead{{ID: "gt-run", Status: "hooked", CreatedAt: created}}
	d.runScheduledSlings()
	if len(f.slungIDs) != 0 || len(f.closedIDs) != 0 {
		t.Fatalf("a hooked run is in flight: slung %v closed %v", f.slungIDs, f.closedIDs)
	}
	f.beads[0].Status = "open"
	f.beads[0].ResumeBranch = "polecat/nux/gt-run+abc"

	d.runScheduledSlings()
	if len(f.closedIDs) != 0 {
		t.Fatalf("a reopened run must not be retired as stalled, closed %v (%v)", f.closedIDs, f.closedWhy)
	}
	if len(f.created) != 0 {
		t.Errorf("a reopened run must not be replaced by a fresh run, created %v", f.created)
	}
	if len(f.slungIDs) != 1 || f.slungIDs[0] != "gt-run" || f.slungBranches[0] != "polecat/nux/gt-run+abc" {
		t.Fatalf("the reopened run must be slung again on its preserved branch, slung %v on %v", f.slungIDs, f.slungBranches)
	}
	if got := d.scheduledSlingFailures["doc-audit"]; got != 0 {
		t.Errorf("a resumed run is not a failure, got %d", got)
	}

	// Hooked again, the run is in flight: the next tick leaves it alone.
	d.runScheduledSlings()
	if len(f.slungIDs) != 1 || len(f.closedIDs) != 0 || len(f.created) != 0 {
		t.Errorf("the resumed run is in flight: slung %v closed %v created %v", f.slungIDs, f.closedIDs, f.created)
	}
	if len(*esc) != 0 {
		t.Errorf("nothing failed, escalated %v", *esc)
	}
}

// A resume branch deleted from origin out-of-band has no work to continue and
// would fail every tick: the run is slung from main instead. A probe that
// cannot answer is not "gone", so the resume goes ahead on the branch as before
// (gt-84o0a).
func TestRunScheduledSlings_AResumeBranchGoneFromOriginIsSlungFromMain(t *testing.T) {
	t.Parallel()
	const branch = "polecat/nux/gt-run+abc"
	for _, tc := range []struct {
		name       string
		gone       []string
		probeErr   error
		wantBranch string
	}{
		{name: "branch on origin", wantBranch: branch},
		{name: "branch gone from origin", gone: []string{branch}, wantBranch: ""},
		{name: "another branch gone", gone: []string{"polecat/other/x"}, wantBranch: branch},
		{name: "probe fails", probeErr: errors.New("origin unreachable"), wantBranch: branch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-next", now: time.Now(), goneBranches: tc.gone, probeErr: tc.probeErr}
			d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
			f.beads = []scheduledBead{{ID: "gt-run", Status: "open", CreatedAt: time.Now().Add(-time.Hour), ResumeBranch: branch}}
			d.runScheduledSlings()
			if len(f.slungIDs) != 1 || f.slungIDs[0] != "gt-run" || f.slungBranches[0] != tc.wantBranch {
				t.Fatalf("slung %v on %q, want gt-run on %q", f.slungIDs, f.slungBranches, tc.wantBranch)
			}
			if len(f.closedIDs) != 0 || len(f.created) != 0 {
				t.Errorf("the reopened bead is slung in place: closed %v created %v", f.closedIDs, f.created)
			}
			if got := d.scheduledSlingFailures["doc-audit"]; got != 0 || len(*esc) != 0 {
				t.Errorf("a sling that succeeded is no failure: count %d, escalations %v", got, *esc)
			}
		})
	}
}

// A fresh sling that fails keeps the bead open and counts, like any failed sling.
func TestRunScheduledSlings_AFailedFreshResumeStillCounts(t *testing.T) {
	t.Parallel()
	const branch = "polecat/nux/gt-run+abc"
	f := &fakeScheduledRunner{createID: "gt-next", now: time.Now(), goneBranches: []string{branch}, slingErr: errors.New("boom")}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	f.beads = []scheduledBead{{ID: "gt-run", Status: "open", CreatedAt: time.Now().Add(-time.Hour), ResumeBranch: branch}}
	d.runScheduledSlings()
	if got := d.scheduledSlingFailures["doc-audit"]; got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}
	if len(f.closedIDs) != 0 {
		t.Errorf("the bead must stay open for the next tick, closed %v", f.closedIDs)
	}
}

// A resume that fails leaves the reopened bead open and counts toward the
// escalation. Closing it with the failure marker would send the next tick to a
// fresh run from main and strand the branch again.
func TestRunScheduledSlings_AFailedResumeKeepsTheBeadAndEscalates(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-next", now: time.Now(), slingErr: errors.New("branch not found")}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	f.beads = []scheduledBead{{ID: "gt-run", Status: "open", CreatedAt: time.Now().Add(-time.Hour), ResumeBranch: "polecat/nux/gt-run+abc"}}
	for i := 1; i <= 3; i++ {
		d.runScheduledSlings()
		if got := d.scheduledSlingFailures["doc-audit"]; got != i {
			t.Fatalf("after tick %d: consecutive failures = %d, want %d", i, got, i)
		}
	}
	if len(f.closedIDs) != 0 || len(f.created) != 0 {
		t.Errorf("a failed resume must keep the bead and start no fresh run: closed %v created %v", f.closedIDs, f.created)
	}
	if len(*esc) != 1 || !strings.Contains((*esc)[0], "polecat/nux/gt-run+abc") {
		t.Errorf("the third failure must escalate naming the branch, got %v", *esc)
	}
}

// The failure error names where the sling went: the resume branch, or main when
// the branch was gone from origin and the run was slung from main (gt-t4j0m).
func TestResumeReopenedScheduledRun_FailureNamesTheTargetSlung(t *testing.T) {
	t.Parallel()
	const branch = "polecat/nux/gt-run+abc"
	for _, tc := range []struct {
		name    string
		gone    []string
		want    string
		wantNot string
	}{
		{name: "branch on origin", want: "on branch " + branch, wantNot: "from main"},
		{name: "branch gone from origin", gone: []string{branch}, want: "from main", wantNot: branch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-next", now: time.Now(), goneBranches: tc.gone, slingErr: errors.New("boom")}
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
			b := scheduledBead{ID: "gt-run", Status: "open", CreatedAt: time.Now().Add(-time.Hour), ResumeBranch: branch}
			err := d.resumeReopenedScheduledRun(context.Background(), f, docAuditEntry, b)
			if err == nil {
				t.Fatal("resumeReopenedScheduledRun returned nil for a failed sling")
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.wantNot) {
				t.Errorf("error = %q, want it to contain %q and not %q", err, tc.want, tc.wantNot)
			}
		})
	}
}

// A capacity refusal of the resume is a deferral, not a failure: the bead stays
// open and the next tick asks again.
func TestRunScheduledSlings_ARefusedResumeIsADeferral(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-next", now: time.Now(), slingErr: errors.New(capacityRefusals["pool full"])}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	f.beads = []scheduledBead{{ID: "gt-run", Status: "open", CreatedAt: time.Now().Add(-time.Hour), ResumeBranch: "polecat/nux/gt-run+abc"}}
	for i := 0; i < 3; i++ {
		d.runScheduledSlings()
	}
	if got := d.scheduledSlingFailures["doc-audit"]; got != 0 || len(*esc) != 0 {
		t.Errorf("a refusal must leave the failure count alone: count %d escalated %v", got, *esc)
	}
	if len(f.closedIDs) != 0 {
		t.Errorf("a refused resume keeps the bead, closed %v", f.closedIDs)
	}
}

// The decision half: a reopened run is resumed at any age, a bead no sling
// hooked is still retired, and a spec-dispatched entry (staleAfter 0) leaves
// the reopened bead to the dispatcher.
func TestDecideScheduledSling_ReopenedRunIsResumedNotRetired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	week := 168 * time.Hour
	const staleAfter = 20 * time.Minute
	old := now.Add(-30 * 24 * time.Hour)
	reopened := scheduledBead{ID: "gt-reopened", Status: "open", CreatedAt: old, ResumeBranch: "polecat/nux/x"}
	neverStarted := scheduledBead{ID: "gt-never", Status: "open", CreatedAt: old}
	cases := []struct {
		name       string
		beads      []scheduledBead
		staleAfter time.Duration
		want       scheduledAction
		resume     string
		stale      int
	}{
		{"reopened past the budget", []scheduledBead{reopened}, staleAfter, scheduledResume, "gt-reopened", 0},
		{"reopened inside the budget", []scheduledBead{{ID: "gt-r", Status: "open", CreatedAt: now.Add(-time.Minute), ResumeBranch: "b"}}, staleAfter, scheduledResume, "gt-r", 0},
		{"never started still retires", []scheduledBead{neverStarted}, staleAfter, scheduledRecoverStale, "", 1},
		{"reopened beside a never-started bead", []scheduledBead{neverStarted, reopened}, staleAfter, scheduledResume, "gt-reopened", 0},
		{"reopened with no readable created_at", []scheduledBead{{ID: "gt-r", Status: "open", ResumeBranch: "b"}}, staleAfter, scheduledResume, "gt-r", 0},
		{"hooked with a stale note is in flight", []scheduledBead{{ID: "gt-h", Status: "hooked", CreatedAt: old, ResumeBranch: "b"}}, staleAfter, scheduledSkipOpen, "", 0},
		{"a young run in flight beside a reopened one", []scheduledBead{{ID: "gt-h", Status: "hooked", CreatedAt: now}, reopened}, staleAfter, scheduledSkipOpen, "", 0},
		{"spec entry leaves it to the dispatcher", []scheduledBead{reopened}, 0, scheduledSkipOpen, "", 0},
	}
	for _, c := range cases {
		dec := decideScheduledSling(c.beads, week, c.staleAfter, now)
		if dec.action != c.want || dec.resume.ID != c.resume || len(dec.stale) != c.stale {
			t.Errorf("%s: got action %v resume %q stale %d, want %v %q %d", c.name, dec.action, dec.resume.ID, len(dec.stale), c.want, c.resume, c.stale)
		}
	}
}

func TestScheduledBeadsOf_ReadsTheResumeBranchNote(t *testing.T) {
	t.Parallel()
	got := scheduledBeadsOf([]*beads.Issue{
		{ID: "gt-a", Status: "open", Notes: "spec-dispatch: x\nresume_branch: polecat/nux/gt-a+1\nresume_branch: polecat/nux/gt-a+2"},
		{ID: "gt-b", Status: "open"},
	})
	if got[0].ResumeBranch != "polecat/nux/gt-a+2" || got[1].ResumeBranch != "" {
		t.Errorf("resume branches = %q, %q; want the newest note's branch and none", got[0].ResumeBranch, got[1].ResumeBranch)
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
	if got := decideScheduledSling(f.beads, scheduledSlingInterval(docAuditEntry), 0, time.Now()).action; got != scheduledDispatch {
		t.Errorf("the next tick after a failed sling = %v, want dispatch", got)
	}
}

// capacityRefusals are the lines gt sling prints when it will not take a bead
// for lack of room, as the pool guard and the merge-queue guard word them.
var capacityRefusals = map[string]string{
	"pool full":    "gt sling gt-x: exit status 1: sling refused: polecat pool full (2/2 claude-sonnet seats); raise polecat_pool.seats.claude-sonnet.cap to spawn",
	"total full":   "gt sling gt-x: exit status 1: sling refused: polecat pool total full (4/4 seats); raise polecat_pool.max_total to spawn",
	"merge queue":  "gt sling gt-x: exit status 1: sling refused: merge queue has 5 ready; raise merge_queue.max_ready_for_dispatch",
	"cobra prefix": "gt sling gt-x: exit status 1: Error: sling refused: polecat pool full (2/2 seats); raise polecat_pool.seats.claude-sonnet.cap to spawn",
}

func TestRunScheduledSlingEntry_CapacityRefusalIsADeferralAndRetiresTheBead(t *testing.T) {
	t.Parallel()
	for name, refusal := range capacityRefusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New(refusal), now: time.Now()}
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
			err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now())
			var deferred *scheduledSlingDeferred
			if !errors.As(err, &deferred) {
				t.Fatalf("a capacity refusal must come back as a deferral, got %v", err)
			}
			if len(f.closedIDs) != 1 || f.closedIDs[0] != "gt-x" || !strings.HasPrefix(f.closedWhy[0], scheduledSlingFailureMarker) {
				t.Fatalf("the refused run bead must still be retired with the failure marker, closed %v for %v", f.closedIDs, f.closedWhy)
			}
			if got := decideScheduledSling(f.beads, scheduledSlingInterval(docAuditEntry), 0, time.Now()).action; got != scheduledDispatch {
				t.Errorf("the next tick after a refused sling = %v, want dispatch", got)
			}
		})
	}
}

// TestRunScheduledSlingEntry_CapacityRefusalCreatesNoBead is the gt-ttqq8
// regression for the churn: the run bead is created before the sling, so a
// refusal at capacity used to leave one bead created and failed-closed on every
// tick the town stayed full — 96 a day for a single entry. The pre-flight
// answers before the bead exists, so a full town costs the patrol a probe and
// nothing in the beads store.
func TestRunScheduledSlingEntry_CapacityRefusalCreatesNoBead(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{
		createID:    "gt-run1",
		now:         time.Now(),
		admitReason: "sling refused: polecat pool full (2/2 claude-sonnet seats); raise polecat_pool.seats.claude-sonnet.cap to spawn",
	}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	before := len(f.beads)
	for tick := 1; tick <= 2; tick++ {
		err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, now.Add(time.Duration(tick)*scheduledSlingsTickInterval))
		var deferred *scheduledSlingDeferred
		if !errors.As(err, &deferred) {
			t.Fatalf("tick %d: a capacity refusal must come back as a deferral, got %v", tick, err)
		}
	}
	if len(f.beads) != before {
		t.Errorf("bead count = %d after two refused ticks, want %d", len(f.beads), before)
	}
	if len(f.created) != 0 || len(f.closedIDs) != 0 {
		t.Errorf("created %v and closed %v; a refused capacity check leaves no bead behind", f.created, f.closedIDs)
	}
	if len(f.slungIDs) != 0 {
		t.Errorf("slung %v; a refused capacity check never reaches the sling", f.slungIDs)
	}
	if len(f.admitted) != 2 {
		t.Errorf("the pre-flight ran %d time(s), want once per tick", len(f.admitted))
	}
}

// TestRunScheduledSlingEntry_CapacityProbeThatCannotAnswerDispatches: a probe
// the town cannot answer is not a refusal — the pool's own posture on an
// uncountable seat read — so the tick dispatches as before and the sling's
// refusal is the backstop (gt-ttqq8).
func TestRunScheduledSlingEntry_CapacityProbeThatCannotAnswerDispatches(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{
		createID: "gt-run1",
		now:      time.Now(),
		admitErr: errors.New("gt sling --dry-run: exit status 1: formula 'mol-doc-audit' not found"),
	}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)

	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatalf("an unanswerable probe must not block the dispatch: %v", err)
	}
	if len(f.admitted) != 1 {
		t.Errorf("the pre-flight ran %d time(s), want once", len(f.admitted))
	}
	if len(f.created) != 1 || len(f.slungIDs) != 1 {
		t.Errorf("created %v and slung %v; an unanswerable probe dispatches as before", f.created, f.slungIDs)
	}
}

// TestRunScheduledSlingEntry_SpecDispatchSkipsTheCapacityProbe: the dispatcher
// slings a spec-dispatched run, not this patrol, so the entry has no capacity
// of its own to pre-check and the probe must not run — a full pool is no reason
// to withhold a bead the dispatcher will queue regardless (gt-ttqq8).
func TestRunScheduledSlingEntry_SpecDispatchSkipsTheCapacityProbe(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now(), admitReason: capacityRefusals["pool full"]}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{specAuditEntry}, f)

	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	if err := d.runScheduledSlingEntry(context.Background(), specAuditEntry, now); err != nil {
		t.Fatalf("a spec-dispatched run must be created with the pool full: %v", err)
	}
	if len(f.admitted) != 0 {
		t.Errorf("the capacity pre-flight ran for a spec-dispatched entry (%v); this patrol does not sling it", f.admitted)
	}
	if len(f.created) != 1 || len(f.slungIDs) != 0 {
		t.Errorf("created %v and slung %v; want the run bead created for the dispatcher and no sling", f.created, f.slungIDs)
	}
}

func TestRunScheduledSlingEntry_NonCapacitySlingFailureIsNotADeferral(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New("gt sling gt-x: exit status 1: worktree verification failed"), now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now())
	var deferred *scheduledSlingDeferred
	if err == nil || errors.As(err, &deferred) {
		t.Fatalf("a real sling failure must stay a failure, got %v", err)
	}
}

func TestRunScheduledSlings_CapacityRefusalDoesNotCountOrEscalate(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New(capacityRefusals["pool full"]), now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	for i := 1; i <= scheduledSlingEscalateAfter+1; i++ {
		d.runScheduledSlings()
		if got := d.scheduledSlingFailures["doc-audit"]; got != 0 {
			t.Fatalf("after refused tick %d: failure count = %d, want 0", i, got)
		}
	}
	if len(*esc) != 0 {
		t.Fatalf("capacity refusals must not escalate, got %v", *esc)
	}
	if len(f.slungIDs) != scheduledSlingEscalateAfter+1 {
		t.Errorf("the entry must be retried every tick, slung %d times", len(f.slungIDs))
	}
	if len(f.closedIDs) != len(f.slungIDs) {
		t.Errorf("every refused run bead must be retired, closed %v", f.closedIDs)
	}
}

func TestRunScheduledSlings_ADeferralLeavesTheFailureCountAlone(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New("boom"), now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	d.runScheduledSlings()
	f.slingErr = errors.New(capacityRefusals["total full"])
	d.runScheduledSlings()
	if got := d.scheduledSlingFailures["doc-audit"]; got != 1 {
		t.Fatalf("a deferral between failures must neither add nor reset, count = %d, want 1", got)
	}
	f.slingErr = errors.New("boom")
	d.runScheduledSlings()
	d.runScheduledSlings()
	if len(*esc) != 1 {
		t.Fatalf("three real failures around a deferral must escalate once, got %v", *esc)
	}
}

// TestScheduledSlingParkedRigDefers: sling.Blocked refuses a parked or docked
// rig with a line that carries no "sling refused:" marker, and the patrol sees
// only that text. It is a deferral — logged, the failure counter untouched, no
// escalation however many ticks it lasts — as the e-stop on the same entry is
// (gt-d1wsm).
func TestScheduledSlingParkedRigDefers(t *testing.T) {
	t.Parallel()
	const refusal = `gt sling gt-x: exit status 1: Error: cannot sling to parked rig "x"`
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New(refusal), now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	for range scheduledSlingEscalateAfter + 1 {
		d.runScheduledSlings()
	}
	if got := d.scheduledSlingFailures["doc-audit"]; got != 0 {
		t.Errorf("scheduledSlingFailures = %d, want 0 for a parked rig", got)
	}
	if len(*esc) != 0 {
		t.Errorf("a parked rig must never escalate, got %v", *esc)
	}
	if !strings.Contains(logs.String(), "deferred: "+refusal) {
		t.Errorf("want a deferral logged, log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "failure ") {
		t.Errorf("a parked rig is not a failure; log:\n%s", logs.String())
	}
}

// TestScheduledSlingDuplicateContentDefers: a run the duplicate-content guard
// refuses for overlapping work in flight is a refusal, the same as a parked rig:
// the patrol logs a deferral and the failure counter stays put, however many
// ticks it lasts (gt-g1xiy). The error is the exec runner's wrapping of the
// closing line of the report, as the subprocess's stderr delivers it.
func TestScheduledSlingDuplicateContentDefers(t *testing.T) {
	t.Parallel()
	const refusal = "gt sling gt-x: exit status 1: Refusing to sling gt-x: 1 overlapping bead(s) share the test(s) above; re-sling with --force if this is genuinely distinct: gt sling gt-x <target> --force"
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New(refusal), now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	for range scheduledSlingEscalateAfter + 1 {
		d.runScheduledSlings()
	}
	if got := d.scheduledSlingFailures["doc-audit"]; got != 0 {
		t.Errorf("failure count = %d, want 0: a duplicate-content refusal is not a failed run", got)
	}
	if len(*esc) != 0 {
		t.Errorf("duplicate-content refusals must not escalate, got %v", *esc)
	}
	if !strings.Contains(logs.String(), "deferred") {
		t.Errorf("want a deferral logged, log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "failure ") {
		t.Errorf("a duplicate-content refusal is not a failure; log:\n%s", logs.String())
	}
}

func TestSlingErrorLineKeepsTheRefusalLine(t *testing.T) {
	t.Parallel()
	stderr := "[sling] step admission took 1s (total 1s)\n" +
		"warning: stale hook file ignored\n" +
		"Error: sling refused: polecat pool full (2/2 seats); raise polecat_pool.seats.claude-sonnet.cap to spawn\n"
	got := slingErrorLine(stderr)
	if !isScheduledSlingRefusal(errors.New(got)) {
		t.Fatalf("summary %q lost the refusal marker", got)
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
	var logs strings.Builder
	d := &Daemon{logger: log.New(&logs, "", 0)} // patrol inactive: a started cycle returns at once
	const skipped = "previous cycle still running"
	d.scheduledSlingsRunning.Store(true)
	d.triggerScheduledSlings()
	if !strings.Contains(logs.String(), skipped) {
		t.Error("a trigger must skip while a cycle is running")
	}
	logs.Reset()
	d.scheduledSlingsRunning.Store(false)
	d.triggerScheduledSlings()
	awaitScheduledSlingsIdle(t, d)
	if strings.Contains(logs.String(), skipped) {
		t.Error("a trigger with no cycle running should start one")
	}
}

func TestExecScheduledRunner_SlingArgv(t *testing.T) {
	t.Parallel()
	r := &execScheduledSlingRunner{townRoot: "/town", bdPath: "bd", gtPath: "gt"}
	e := docAuditEntry
	e.Vars = map[string]string{"slice_docs": "8"}
	got := r.slingArgs("gt-run1", "", e)
	want := []string{"sling", "gt-run1", "gastown", "--formula=mol-doc-audit", "--agent=deepseek-pro", "--actor=daemon/scheduled:doc-audit", "--no-boot", "--var", "slice_docs=8"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv\n got %v\nwant %v", got, want)
	}

	// A resume adds --branch beside the agent flag, before the actor.
	got = r.slingArgs("gt-run1", "polecat/nux/x", e)
	want = []string{"sling", "gt-run1", "gastown", "--formula=mol-doc-audit", "--agent=deepseek-pro", "--branch=polecat/nux/x", "--actor=daemon/scheduled:doc-audit", "--no-boot", "--var", "slice_docs=8"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("resume argv\n got %v\nwant %v", got, want)
	}
}

// TestExecScheduledSlingRunner_AdmitReadsTheDryRunRoute: the real runner asks
// `gt sling --dry-run` for the route, and reads the refusal out of the preview
// on stdout — where a dry run reports — through the same marker the live sling
// puts on stderr (gt-ttqq8).
func TestExecScheduledSlingRunner_AdmitReadsTheDryRunRoute(t *testing.T) {
	t.Parallel()
	entry := docAuditEntry
	cases := map[string]struct {
		stdout  string
		stderr  string
		cmdErr  error
		reason  string
		wantErr bool
	}{
		"refused seat pool": {
			stdout: "Would spawn fresh polecat in rig 'gastown'\n  sling refused: polecat pool full (2/2 seats); raise polecat_pool.seats.claude-sonnet.cap to spawn\n",
			reason: "sling refused: polecat pool full (2/2 seats); raise polecat_pool.seats.claude-sonnet.cap to spawn",
		},
		"parked rig": {
			stderr: "Error: cannot sling to parked rig \"gastown\"\ngt rig unpark gastown\n",
			cmdErr: errors.New("exit status 1"),
			reason: `cannot sling to parked rig "gastown"`,
		},
		"seat free": {
			stdout: "Would spawn fresh polecat in rig 'gastown'\nWould cook formula: mol-doc-audit\n",
		},
		"probe cannot answer": {
			stderr:  "Error: formula 'mol-doc-audit' not found\n",
			cmdErr:  errors.New("exit status 1"),
			wantErr: true,
		},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got *exec.Cmd
			r := &execScheduledSlingRunner{townRoot: "/town", gtPath: "gt", execCmd: func(cmd *exec.Cmd) ([]byte, []byte, error) {
				got = cmd
				return []byte(tt.stdout), []byte(tt.stderr), tt.cmdErr
			}}
			reason, err := r.admit(context.Background(), entry)
			if tt.wantErr && err == nil {
				t.Fatalf("a probe that cannot answer must report an error, got reason %q", reason)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("admit: %v", err)
			}
			if reason != tt.reason {
				t.Errorf("reason = %q, want %q", reason, tt.reason)
			}
			want := []string{"sling", "mol-doc-audit", "gastown", "--dry-run", "--no-boot", "--agent=deepseek-pro", "--actor=daemon/scheduled:doc-audit"}
			if got == nil {
				t.Fatalf("the probe ran no command; want %v", want)
			}
			if strings.Join(got.Args[1:], " ") != strings.Join(want, " ") {
				t.Errorf("argv = %v, want %v", got.Args, want)
			}
			if got.Dir != "/town" {
				t.Errorf("dir = %q, want the town root", got.Dir)
			}
		})
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

	id, err := r.createBead(ctx, "gastown", scheduledRunBead{Title: "doc audit", Labels: []string{"scheduled:doc-audit"}, Description: "run body", Priority: 3})
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

// The scheduled sling runs `gt sling` as a subprocess and logs one line of its
// stderr when it fails. With the step timing lines on stderr that line would be
// "[sling] step admission took ..." and the real error would be lost, so the
// summary skips them (om review of gt-llg8, major).
func TestSlingErrorLineSkipsTimingLines(t *testing.T) {
	t.Parallel()
	stderr := "[sling] step admission took 1.2s (total 1.2s)\n" +
		"[sling] step allocate took 3s (total 4.2s)\n" +
		"Error: worktree verification failed for opal: no such directory\n" +
		"Hint: try gt polecat nuke\n"
	if got, want := slingErrorLine(stderr), "Error: worktree verification failed for opal: no such directory"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSlingErrorLineFallsBackToFirstLine(t *testing.T) {
	t.Parallel()
	if got := slingErrorLine("[sling] step admission took 1s (total 1s)\n"); got != "[sling] step admission took 1s (total 1s)" {
		t.Fatalf("got %q", got)
	}
	if got := slingErrorLine(""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// awaitScheduledSlingsIdle waits for the goroutine triggerScheduledSlings
// starts; the atomic flag's release orders the fake runner's writes before the
// test reads them.
func awaitScheduledSlingsIdle(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for d.scheduledSlingsRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("scheduled_slings cycle did not finish")
		}
		runtime.Gosched()
	}
}

// The startup catch-up is one triggerScheduledSlings call with no tick before
// it: a due schedule dispatches then, and the tick that follows reads the run
// bead the startup call created and does not dispatch a second time (gt-jok8h).
func TestTriggerScheduledSlings_StartupCatchUpDispatchesDueOnce(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)

	d.triggerScheduledSlings()
	awaitScheduledSlingsIdle(t, d)
	if len(f.slungIDs) != 1 || f.slungIDs[0] != "gt-run1" {
		t.Fatalf("startup trigger sling calls = %v, want one dispatch of gt-run1", f.slungIDs)
	}

	d.triggerScheduledSlings() // the first tick, after the startup call
	awaitScheduledSlingsIdle(t, d)
	if len(f.slungIDs) != 1 || len(f.created) != 1 {
		t.Errorf("tick after startup dispatched again: created=%v sling=%v", f.created, f.slungIDs)
	}
}

// The startup catch-up honors the shutdown.lock guard the tick path applies: a
// daemon started while `gt down` holds the lock dispatches nothing (gt-84o0a).
func TestCatchUpScheduledSlings_HonorsTheShutdownLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		holdLock     bool
		wantDispatch bool
	}{
		{"no shutdown", false, true},
		{"shutdown.lock held", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
			if tc.holdLock {
				lockPath := filepath.Join(d.config.TownRoot, "daemon", "shutdown.lock")
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
					t.Fatal(err)
				}
				lock := flock.New(lockPath)
				if locked, err := lock.TryLock(); err != nil || !locked {
					t.Fatalf("TryLock = %v, %v", locked, err)
				}
				t.Cleanup(func() { _ = lock.Unlock() })
			}
			d.catchUpScheduledSlings()
			awaitScheduledSlingsIdle(t, d)
			if dispatched := len(f.slungIDs) == 1; dispatched != tc.wantDispatch {
				t.Errorf("dispatched = %v, want %v (created %v, slung %v)", dispatched, tc.wantDispatch, f.created, f.slungIDs)
			}
		})
	}
}

// A cycle whose Dolt bring-up was skipped for shutdown does not run its pass:
// reading bead history or opening SQL connections needs the server the
// shutdown is stopping (gt-84o0a).
func TestTriggerScheduledSlings_SkipsThePassWhenDoltBringUpIsSkipped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		shuttingDown bool
		wantDispatch bool
	}{
		{"daemon running", false, true},
		{"daemon shutting down", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var starts atomic.Int32
			var logs strings.Builder
			f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
			d.logger = log.New(&logs, "", 0)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			d.ctx, d.cancel = ctx, cancel
			d.doltServer = shuttingDownTestManager(t, d, &starts)
			if tc.shuttingDown {
				cancel()
			}
			d.triggerScheduledSlings()
			awaitScheduledSlingsIdle(t, d)
			if dispatched := len(f.slungIDs) == 1; dispatched != tc.wantDispatch {
				t.Errorf("dispatched = %v, want %v", dispatched, tc.wantDispatch)
			}
			if strings.Contains(logs.String(), "Dolt server unavailable") {
				t.Errorf("a shutdown skip is not an unavailable server; log:\n%s", logs.String())
			}
		})
	}
}

func TestTriggerScheduledSlings_StartupCatchUpSkipsRecentRun(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{beads: []scheduledBead{{ID: "gt-old", Status: "closed", CreatedAt: time.Now().Add(-time.Hour)}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)

	d.triggerScheduledSlings()
	awaitScheduledSlingsIdle(t, d)
	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("recent run bead must suppress the startup dispatch, got create=%v sling=%v", f.created, f.slungIDs)
	}
}
