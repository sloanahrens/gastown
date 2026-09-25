package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduledSlingEntry_Validate(t *testing.T) {
	good := ScheduledSlingEntry{Name: "doc-audit", Rig: "gastown", Formula: "mol-doc-audit", IntervalStr: "168h"}
	if err := good.validate(); err != nil {
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
		if err := e.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := good.label(); got != "scheduled:doc-audit" {
		t.Errorf("label = %q", got)
	}
	if got := good.priority(); got != 3 {
		t.Errorf("default priority = %d, want 3", got)
	}
	if got := good.interval(); got != 168*time.Hour {
		t.Errorf("interval = %v", got)
	}
}

func TestDecideScheduledSling(t *testing.T) {
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

func TestParseScheduledBeads(t *testing.T) {
	data := []byte(`[{"id":"gt-abc","status":"closed","created_at":"2026-09-12T10:00:00Z","labels":["scheduled:doc-audit"]},
	                 {"id":"gt-def","status":"open","created_at":"2026-09-19T10:00:00-05:00"}]`)
	got, err := parseScheduledBeads(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gt-abc" || got[1].Status != "open" {
		t.Fatalf("parsed %+v", got)
	}
	if !got[1].CreatedAt.Equal(time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at not parsed with offset: %v", got[1].CreatedAt)
	}
	if _, err := parseScheduledBeads([]byte(`not json`)); err == nil {
		t.Error("expected error on bad json")
	}
	empty, err := parseScheduledBeads([]byte(`[]`))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty list: %v %v", empty, err)
	}
}

func TestParseCreatedBeadID(t *testing.T) {
	for _, in := range []string{`{"id":"gt-new1","title":"x"}`, `[{"id":"gt-new1","title":"x"}]`} {
		id, err := parseCreatedBeadID([]byte(in))
		if err != nil || id != "gt-new1" {
			t.Errorf("%s: id=%q err=%v", in, id, err)
		}
	}
	if _, err := parseCreatedBeadID([]byte(`{}`)); err == nil {
		t.Error("expected error when id is missing")
	}
}

func TestIsPatrolEnabled_ScheduledSlingsIsOptIn(t *testing.T) {
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
	// bd/Dolt can emit RFC3339Nano, which encoding/json's strict RFC3339
	// time.Time unmarshaller rejects — one such row used to fail the whole list
	// and feed the spurious-escalation path.
	data := []byte(`[{"id":"gt-nano","status":"closed","created_at":"2026-09-12T10:00:00.123456789Z"}]`)
	got, err := parseScheduledBeads(data)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 12, 10, 0, 0, 123456789, time.UTC)
	if len(got) != 1 || !got[0].CreatedAt.Equal(want) {
		t.Fatalf("parsed %+v, want created_at %v", got, want)
	}
}

func TestDecideScheduledSling_IgnoresRunsThatFailedToSling(t *testing.T) {
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
	if got := decideScheduledSling(f.beads, docAuditEntry.interval(), time.Now()); got != scheduledDispatch {
		t.Errorf("the next tick after a failed sling = %v, want dispatch", got)
	}
}

func TestRunScheduledSlings_InvalidEntryIsSkippedNotFatal(t *testing.T) {
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
	d := &Daemon{logger: discardLogger} // patrol inactive: run returns at once
	if !d.triggerScheduledSlings() {
		t.Fatal("first trigger should start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.scheduledSlingsRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("first cycle did not clear the flag")
		}
		time.Sleep(time.Millisecond)
	}
	d.scheduledSlingsRunning.Store(true)
	if d.triggerScheduledSlings() {
		t.Error("second trigger must skip while running")
	}
}

func TestExecScheduledRunner_SlingArgv(t *testing.T) {
	r := &execScheduledSlingRunner{townRoot: "/town", bdPath: "bd", gtPath: "gt"}
	e := docAuditEntry
	e.Vars = map[string]string{"slice_docs": "8"}
	got := r.slingArgs("gt-run1", e)
	want := []string{"sling", "gt-run1", "gastown", "--formula=mol-doc-audit", "--agent=deepseek-pro", "--actor=daemon/scheduled:doc-audit", "--no-boot", "--var", "slice_docs=8"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv\n got %v\nwant %v", got, want)
	}
}

// TestExecScheduledSlingRunner_ListBeadsArgvContract runs the real argv through
// a stub bd that models the two behaviours the patrol depends on: bd v0.59+
// needs --flat for --json to emit JSON at all, and bd's default list filter
// hides closed issues. Dropping either makes the newest completed run invisible
// and the interval guard meaningless — the two integration bugs that unit tests
// over hand-crafted JSON could not see.
func TestExecScheduledSlingRunner_ListBeadsArgvContract(t *testing.T) {
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	payload := `[{"id":"gt-done","status":"closed","close_reason":"audit complete","created_at":"2026-09-12T10:00:00.5Z"},` +
		`{"id":"gt-open","status":"open","created_at":"2026-09-11T10:00:00Z"}]`
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "bd.log")

	stub := `#!/bin/sh
{
  for a in "$@"; do printf 'arg %s\n' "$a"; done
} >> ` + shellQuote(logPath) + `

if [ "$1" != "list" ]; then exit 0; fi

has_flat=0
has_all=0
for a in "$@"; do
  [ "$a" = "--flat" ] && has_flat=1
  [ "$a" = "--all" ] && has_all=1
done

# bd v0.59+: without --flat, --json still prints human-readable tree text.
if [ "$has_flat" != 1 ]; then
  echo 'gt-done  [closed] doc-audit 2026-09-12'
  exit 0
fi

if [ "$has_all" = 1 ]; then
  cat ` + shellQuote(payloadPath) + `
else
  # bd's default filter excludes closed issues.
  printf '[{"id":"gt-open","status":"open","created_at":"2026-09-11T10:00:00Z"}]'
fi
exit 0
`
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "bd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &execScheduledSlingRunner{townRoot: townRoot, bdPath: filepath.Join(stubDir, "bd"), gtPath: "gt"}
	got, err := r.listBeads(context.Background(), "gastown", docAuditEntry.label())
	if err != nil {
		t.Fatalf("listBeads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listBeads returned %d beads, want 2 — the closed run is invisible, so the interval guard is defeated: %+v", len(got), got)
	}
	if !got[0].CreatedAt.Equal(time.Date(2026, 9, 12, 10, 0, 0, 500000000, time.UTC)) {
		t.Errorf("closed run created_at = %v", got[0].CreatedAt)
	}
	if got[0].Status != "closed" || got[0].CloseReason != "audit complete" {
		t.Errorf("closed run = %+v", got[0])
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"arg --json", "arg --flat", "arg --all", "arg --label"} {
		if !strings.Contains(string(logged), want+"\n") {
			t.Errorf("bd argv is missing %q; logged:\n%s", want, logged)
		}
	}
}
