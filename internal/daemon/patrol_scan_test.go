package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/supervisor"
)

func TestPatrolScanDefaultsOff(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "patrol_scan") {
		t.Error("patrol_scan must be off with no config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "patrol_scan") {
		t.Error("patrol_scan must be off with no patrol_scan entry")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "patrol_scan") {
		t.Error("patrol_scan enabled:true must be on")
	}
}

func TestPatrolScanInterval(t *testing.T) {
	t.Parallel()
	if got := patrolScanInterval(nil); got != 2*time.Minute {
		t.Errorf("default = %v", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{IntervalStr: "90s"}}}
	if got := patrolScanInterval(cfg); got != 90*time.Second {
		t.Errorf("configured = %v", got)
	}
	cfg.Patrols.PatrolScan.IntervalStr = "bogus"
	if got := patrolScanInterval(cfg); got != 2*time.Minute {
		t.Errorf("bad interval = %v", got)
	}
}

// The patrol scan repeats the landing label to stay out of the land
// package's dependency tree; this pins the copy to the original.
func TestPatrolScanReadyToLandLabelMatchesLand(t *testing.T) {
	t.Parallel()
	if patrolscan.ReadyToLandLabel != land.LabelReadyToLand {
		t.Fatalf("patrolscan.ReadyToLandLabel = %q, land.LabelReadyToLand = %q", patrolscan.ReadyToLandLabel, land.LabelReadyToLand)
	}
}

// The patrol scan repeats the DEFERRED exit type to stay out of the done
// package's dependency tree; this pins the copy to the original. It is the
// one exit type the tick restarts on, so a drift here would silently stop or
// start restarting finished turns (gt-ks62m).
func TestPatrolScanExitDeferredMatchesDone(t *testing.T) {
	t.Parallel()
	if patrolscan.ExitDeferred != done.ExitDeferred {
		t.Fatalf("patrolscan.ExitDeferred = %q, done.ExitDeferred = %q", patrolscan.ExitDeferred, done.ExitDeferred)
	}
}

// The patrol scan repeats the ESCALATED exit type because it is the record it
// reads to recognize a seat whose blocker is spent; a drift here would
// silently stop clearing them (gt-fn9e6.33).
func TestPatrolScanExitEscalatedMatchesDone(t *testing.T) {
	t.Parallel()
	if patrolscan.ExitEscalated != done.ExitEscalated {
		t.Fatalf("patrolscan.ExitEscalated = %q, done.ExitEscalated = %q", patrolscan.ExitEscalated, done.ExitEscalated)
	}
}

// The patrol scan repeats the spawn-grace rule and the spawning state it
// turns on to stay out of the polecat package's dependency tree; this pins
// both to the originals, on every state and age the two can be asked about.
// A drift here would read a crashed polecat as spawning — or a spawning one
// as crashed — at whichever site kept its own copy (gt-6hby3).
func TestPatrolScanSpawnGraceMatchesPolecat(t *testing.T) {
	t.Parallel()
	if patrolscan.SpawningAgentState != string(beads.AgentStateSpawning) {
		t.Errorf("patrolscan.SpawningAgentState = %q, beads.AgentStateSpawning = %q",
			patrolscan.SpawningAgentState, beads.AgentStateSpawning)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	states := []string{"spawning", "working", "idle", "stuck", "done", "patrolling", "", " spawning ", "SPAWNING"}
	ages := []time.Duration{0, time.Second, 30 * time.Second, 5 * time.Minute, 6 * time.Minute, time.Hour}
	for _, state := range states {
		for _, age := range ages {
			updated := now.Add(-age)
			got := patrolscan.SpawnGrace(state, updated, now, 5*time.Minute)
			want := polecat.SpawnGrace(state, updated, now, 5*time.Minute)
			if got != want {
				t.Errorf("SpawnGrace(%q, now-%s) = %v, polecat.SpawnGrace says %v", state, age, got, want)
			}
		}
	}
}

// AgentRecord reads the fields that tell a finished turn from a held seat off
// one agent bead: agent_state, the gt done exit type, the cleanup status, the
// hook reference and the source issue the turn ran on. An omitted field is
// empty, not an error.
func TestPatrolScanAgentRecordReadsTheBead(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	const id = "gt-myr-polecat-mycat"
	bd.db.Seed(beads.Issue{ID: id, Description: beads.FormatAgentDescription("mycat", &beads.AgentFields{
		RoleType:        constants.RolePolecat,
		Rig:             "myr",
		AgentState:      "stuck",
		ExitType:        "DEFERRED",
		CleanupStatus:   "has_unpushed",
		HookBead:        "gt-hooked",
		LastSourceIssue: "gt-source",
	})})
	bd.db.Seed(beads.Issue{ID: "gt-myr-polecat-plain", Description: "role_type: polecat\nrig: myr\nagent_state: working\n"})
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}
	h := &patrolScanHost{d: d}

	got, err := h.AgentRecord("myr", "mycat")
	if err != nil {
		t.Fatalf("AgentRecord: %v", err)
	}
	want := patrolscan.AgentRecord{State: "stuck", ExitType: "DEFERRED", CleanupStatus: "has_unpushed",
		HookBead: "gt-hooked", LastSourceIssue: "gt-source"}
	if got != want {
		t.Fatalf("AgentRecord = %+v, want %+v", got, want)
	}
	plain, err := h.AgentRecord("myr", "plain")
	if err != nil {
		t.Fatalf("AgentRecord(plain): %v", err)
	}
	if plain.State != "working" || plain.ExitType != "" || plain.CleanupStatus != "" ||
		plain.HookBead != "" || plain.LastSourceIssue != "" {
		t.Fatalf("AgentRecord(plain) = %+v, want working and no exit metadata", plain)
	}
}

// GitState maps the live worktree probe onto the tick's facts, and turns an
// answer the probe could not give into an error: the tick clears nothing on a
// worktree it did not measure.
//
// The probe must be handed the seat's git worktree, not the polecats/<name>
// container the nested layout puts it inside: the container is not a worktree,
// so probing it never answered and a seat whose escalation had been resolved
// could not reach the clean verdict (gt-okk50). A seat with no worktree at
// all is an error naming what was tried, never a silent clean.
func TestPatrolScanGitState(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	worktree := func(name, inner string) string {
		dir := filepath.Join(town, "myr", "polecats", name)
		if inner != "" {
			dir = filepath.Join(dir, inner)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	nested := worktree("nested", "myr") // the container holds the worktree
	legacy := worktree("legacy", "")    // polecats/<name> is the worktree
	if err := os.MkdirAll(filepath.Join(town, "myr", "polecats", "empty", "myr"), 0o755); err != nil {
		t.Fatal(err) // a container with no .git anywhere: no worktree
	}

	var probed []string
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}},
		gitState: func(path string) polecat.LiveGitState {
			probed = append(probed, path)
			switch path {
			case nested:
				return polecat.LiveGitState{Source: polecat.GitStateSourceLive, Branch: "polecat/nested/gt-a+x",
					Dirty: true, StashCount: 2, UnpushedCommits: 3}
			case legacy:
				return polecat.LiveGitState{Source: polecat.GitStateSourceLive, Branch: "polecat/legacy/gt-b+x"}
			}
			return polecat.LiveGitState{Source: polecat.GitStateSourceUnknown, FailedReason: "git_state=unknown path=" + path + ": not a worktree root"}
		},
	}

	got, err := h.GitState("myr", "nested")
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	want := patrolscan.GitState{Branch: "polecat/nested/gt-a+x", Dirty: true, StashCount: 2, UnpushedCommits: 3}
	if got != want {
		t.Fatalf("GitState = %+v, want %+v", got, want)
	}
	if got, err := h.GitState("myr", "legacy"); err != nil || got.Branch != "polecat/legacy/gt-b+x" {
		t.Fatalf("GitState(legacy) = %+v, %v; want the flat layout's worktree", got, err)
	}
	// A container exists but holds no worktree, and one holds nothing at all:
	// both are errors, and neither reaches the probe.
	for _, name := range []string{"empty", "gone"} {
		_, err := h.GitState("myr", name)
		dir := filepath.Join(town, "myr", "polecats", name)
		if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "myr")) ||
			!strings.Contains(err.Error(), dir) {
			t.Fatalf("GitState(%s) = %v, want an error naming both paths tried", name, err)
		}
	}
	wantProbed := []string{nested, legacy}
	if strings.Join(probed, ",") != strings.Join(wantProbed, ",") {
		t.Fatalf("probed %v, want %v", probed, wantProbed)
	}
}

// PolecatEscalations reads the rig's seats' escalations out of the town
// database, open and closed: the tie the tick matches a stopped seat's turn
// against is escalated_by, so a record another role raised is not one of the
// seat's, and a mail carrier routed for an escalation is not an escalation at
// all (gt-4hduq).
func TestPatrolScanPolecatEscalations(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	escalation := func(id, raiser, title string, status string, labels ...string) beads.Issue {
		return beads.Issue{ID: id, Status: status, Title: title, Labels: append([]string{"gt:escalation"}, labels...),
			Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
				Severity: "high", EscalatedBy: raiser, RelatedBead: "be-adu"}),
		}
	}
	bd.db.Seed(escalation("hq-wisp-open", "myr/polecats/mycat", "be-adu: a decision", "open"))
	bd.db.Seed(escalation("hq-wisp-done", "myr/polecats/mycat", "be-adu: another decision", "closed"))
	bd.db.Seed(escalation("hq-wisp-other-rig", "beads/polecats/guzzle", "be-adu: a decision", "open"))
	bd.db.Seed(escalation("hq-wisp-other-role", "myr/crew/sloan", "be-adu: a decision", "open"))
	bd.db.Seed(escalation("hq-wisp-carrier", "myr/polecats/mycat", "be-adu: a decision", "open", "gt:message"))
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}

	got, err := (&patrolScanHost{d: d}).PolecatEscalations("myr")
	if err != nil {
		t.Fatalf("PolecatEscalations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("escalations = %+v, want the two myr/polecats ones", got)
	}
	byID := map[string]patrolscan.Escalation{}
	for _, e := range got {
		byID[e.ID] = e
		if e.EscalatedBy != "myr/polecats/mycat" {
			t.Errorf("escalation %s raiser = %q, want the rig's polecat", e.ID, e.EscalatedBy)
		}
		if e.RelatedBead != "be-adu" || !strings.HasPrefix(e.Title, "be-adu:") {
			t.Errorf("escalation %s = %+v, want the bead it names", e.ID, e)
		}
	}
	if e, ok := byID["hq-wisp-done"]; !ok || e.Open {
		t.Errorf("closed escalation = %+v, want it read as not open", e)
	}
	if e, ok := byID["hq-wisp-open"]; !ok || !e.Open {
		t.Errorf("open escalation = %+v, want it read as open", e)
	}

	// A failed read is an error, never an empty list: the tick holds a seat on
	// an escalation it could not read.
	bd.listErr = errors.New("bd list: connection refused")
	if _, err := (&patrolScanHost{d: d}).PolecatEscalations("myr"); err == nil {
		t.Fatal("a failed escalation listing must be an error")
	}
}

// ClearStuckExit is the write the tick makes on a resolved escalation: the
// agent bead goes to idle and the exit type is removed, the two fields gt
// done's exit wrote together. The guard is the record still reading as that
// exit, so a seat something else has moved on (a fresh sling, a human) is
// left to it rather than overwritten with idle.
func TestPatrolScanClearStuckExit(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	db := beadsfake.New()
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, "myr"), "myr", "mycat")
	moved := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, "myr"), "myr", "moved")
	db.Seed(beads.Issue{ID: id, Description: beads.FormatAgentDescription("mycat", &beads.AgentFields{
		RoleType: constants.RolePolecat, Rig: "myr", AgentState: "stuck", ExitType: done.ExitEscalated,
		HookBead: "", LastSourceIssue: "be-src"})})
	db.Seed(beads.Issue{ID: moved, Description: beads.FormatAgentDescription("moved", &beads.AgentFields{
		RoleType: constants.RolePolecat, Rig: "myr", AgentState: "working", ExitType: done.ExitEscalated})})
	h := &patrolScanHost{
		d:                 &Daemon{config: &Config{TownRoot: townRoot}},
		openRecoveryBeads: func([]string) beads.Client { return db },
	}

	changed, err := h.ClearStuckExit("myr", "mycat", done.ExitEscalated)
	if err != nil || !changed {
		t.Fatalf("ClearStuckExit = %v, %v; want true, nil", changed, err)
	}
	is, err := db.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	f := beads.ParseAgentFields(is.Description)
	if beads.AgentState(f.AgentState) != beads.AgentStateIdle || f.ExitType != "" {
		t.Fatalf("agent bead = state %q exit %q, want idle and no exit type", f.AgentState, f.ExitType)
	}
	if f.LastSourceIssue != "be-src" {
		t.Fatalf("last_source_issue = %q; the clear must leave the seat's history alone", f.LastSourceIssue)
	}

	// The record no longer reads as an escalated exit: nothing left to clear.
	if changed, err := h.ClearStuckExit("myr", "mycat", done.ExitEscalated); err != nil || changed {
		t.Fatalf("second ClearStuckExit = %v, %v; want false, nil", changed, err)
	}
	// A seat whose state moved on is not the record the tick decided on.
	if changed, err := h.ClearStuckExit("myr", "moved", done.ExitEscalated); err != nil || changed {
		t.Fatalf("ClearStuckExit(moved) = %v, %v; want false, nil", changed, err)
	}
	is, err = db.Show(moved)
	if err != nil {
		t.Fatal(err)
	}
	if got := beads.ParseAgentFields(is.Description).AgentState; got != "working" {
		t.Fatalf("moved seat state = %q, want working", got)
	}
	// A seat with no agent bead is a failed read, never a silent success.
	if _, err := h.ClearStuckExit("myr", "absent", done.ExitEscalated); err == nil {
		t.Fatal("ClearStuckExit on a seat with no bead must fail")
	}
}

// A DEFERRED exit is cleared the same way (gt-oa6du), and only as the exit the
// tick decided on: a record that reads DEFERRED is not cleared by a tick that
// measured an ESCALATED one.
func TestPatrolScanClearStuckExitDeferred(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	db := beadsfake.New()
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, "myr"), "myr", "mycat")
	db.Seed(beads.Issue{ID: id, Description: beads.FormatAgentDescription("mycat", &beads.AgentFields{
		RoleType: constants.RolePolecat, Rig: "myr", AgentState: "stuck", ExitType: done.ExitDeferred,
		LastSourceIssue: "be-src"})})
	h := &patrolScanHost{
		d:                 &Daemon{config: &Config{TownRoot: townRoot}},
		openRecoveryBeads: func([]string) beads.Client { return db },
	}

	if changed, err := h.ClearStuckExit("myr", "mycat", done.ExitEscalated); err != nil || changed {
		t.Fatalf("ClearStuckExit(ESCALATED) on a DEFERRED record = %v, %v; want false, nil", changed, err)
	}
	if changed, err := h.ClearStuckExit("myr", "mycat", done.ExitDeferred); err != nil || !changed {
		t.Fatalf("ClearStuckExit(DEFERRED) = %v, %v; want true, nil", changed, err)
	}
	is, err := db.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	f := beads.ParseAgentFields(is.Description)
	if beads.AgentState(f.AgentState) != beads.AgentStateIdle || f.ExitType != "" || f.LastSourceIssue != "be-src" {
		t.Fatalf("agent bead = state %q exit %q source %q, want idle, no exit type, source kept",
			f.AgentState, f.ExitType, f.LastSourceIssue)
	}
}

// The tick and the daemon's crash detector each ask "is this bead submitted
// for landing?" from their own copy of the rule. gt-xs1ni was two detectors
// answering it differently, so this pins them to the same answer on every
// status a work bead can be in.
func TestPatrolScanIsSubmittedMatchesPolecat(t *testing.T) {
	t.Parallel()
	statuses := []string{"open", "hooked", "in_progress", "blocked", "deferred", "closed", "tombstone", "", "weird"}
	for _, status := range statuses {
		for _, labels := range [][]string{nil, {land.LabelReadyToLand}, {"rework"}, {land.LabelReadyToLand, "rework"}} {
			w := patrolscan.Work{Status: status, Labels: labels}
			got := w.IsSubmitted()
			want := polecat.IsSubmittedWork(&beads.Issue{Status: status, Labels: labels})
			if got != want {
				t.Errorf("status %q labels %v: patrolscan says %v, polecat says %v", status, labels, got, want)
			}
		}
	}
}

func TestPatrolScanOptionsUseDispatchHoldRule(t *testing.T) {
	t.Parallel()
	o := patrolScanOptions(nil, time.Now)
	if why := o.HoldReason(patrolscan.Work{Status: "hooked", Labels: []string{"gt:needs-human"}}); why == "" {
		t.Error("gt:needs-human work must read as held")
	}
	if why := o.HoldReason(patrolscan.Work{Status: "hooked"}); why != "" {
		t.Errorf("plain hooked work read as held: %q", why)
	}
	refused := errors.Join(supervisor.ErrRefused, supervisor.ErrBudgetExhausted)
	if !o.IsRefusal(refused) || o.IsRefusal(errors.New("exit status 1")) {
		t.Error("IsRefusal must match supervisor refusals only")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{DeadSamples: 3, ReportWindow: "6h"}}}
	o = patrolScanOptions(cfg, time.Now)
	if o.DeadSamples != 3 || o.ReportWindow != 6*time.Hour {
		t.Errorf("options = %+v", o)
	}
}

func TestIssueWorkParsesMoleculeAndTime(t *testing.T) {
	t.Parallel()
	w := issueWork(&beads.Issue{
		ID: "gt-a", Status: "hooked", Assignee: "gastown/polecats/ruby",
		Description: "attached_molecule: gt-wisp-6nm\nattached_formula: mol-polecat-work\n",
		UpdatedAt:   "2026-09-30T11:30:06Z",
	})
	if w.AttachedMolecule != "gt-wisp-6nm" {
		t.Errorf("attached molecule = %q", w.AttachedMolecule)
	}
	if w.UpdatedAt.IsZero() {
		t.Error("updated_at not parsed")
	}
}

// A polecat restart in a rig patrol_scan does not cover is declined, so it
// spends no budget and the witness keeps that rig.
func TestRestartPolecatDeclinedOutsidePatrolScan(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: t.TempDir()}}
	err := d.restartSeat(supervisor.SeatFor("gastown", constants.RolePolecat, "ruby"))
	if !errors.Is(err, supervisor.ErrDeclined) {
		t.Fatalf("err = %v, want ErrDeclined", err)
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true, Rigs: []string{"beads"}}}}
	if d.patrolScanActiveForRig("gastown") || !d.patrolScanActiveForRig("beads") {
		t.Fatal("rigs list not honoured")
	}
}

// gt done writes gt:ready-to-land before the intent record, and the record
// write is best-effort: a seat whose write was lost reads as run for the whole
// of the landing, and townhealth reports it dead on the samples the scan
// records while it is mid-landing (gt-2z8k1). Bringing the record up to the
// label ends both.
func TestMarkSubmittedBringsTheRecordUpToTheLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const name = "ruby"
	if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", name), 0o755); err != nil {
		t.Fatal(err)
	}
	iseat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: name}
	writeJSONFile(t, iseat.Path(town), intent.Record{
		Progress: &intent.Progress{SampledAt: time.Now().Add(-time.Minute), DeadSamples: 3},
	})

	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: town}}
	src := &healthSources{d: d, evidence: time.Hour, now: time.Now()}
	before, err := src.Seats()
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}
	if len(before) != 1 || before[0].DeadSamples != 3 {
		t.Fatalf("before = %+v, want the mid-landing seat reported dead", before)
	}

	if err := (&patrolScanHost{d: d}).MarkSubmitted("gastown", name, "gt-ruby"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	rec, err := intent.Read(town, iseat)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Submitted() || rec.WorkBead != "gt-ruby" {
		t.Fatalf("record = %+v, want submitted for gt-ruby", rec)
	}
	if rec.Progress != nil {
		t.Fatalf("progress = %+v, want dropped with the submission", rec.Progress)
	}
	after, err := src.Seats()
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("after = %+v, want the submitted seat dropped", after)
	}
}

func TestTriggerPatrolScanSingleFlight(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.patrolScanRunning.Store(true)
	if d.triggerPatrolScan() {
		t.Fatal("a second tick started while one was running")
	}
	d.patrolScanRunning.Store(false)
	// Patrol off: the tick returns without scanning.
	if !d.triggerPatrolScan() {
		t.Fatal("tick did not start")
	}
	d.patrolScanCycles.Wait()
	if d.patrolScanRunning.Load() {
		t.Fatal("guard not released")
	}
}

// TestPatrolScanGHGatesHonorsTheRecordedRun pins the cadence: a gh gate check
// within the interval dispatches nothing, so bd is never started. The unit
// tier fails the run if an external tool starts, which is the assertion.
func TestPatrolScanGHGatesHonorsTheRecordedRun(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "patrol_scan_gh_gates", time.Now()); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}

	d.patrolScanGHGates(&patrolScanHost{d: d}, nil)
}

// TestPatrolScanGHGatesDoesNotRecordAFailedCheck pins gt-oyrav (D8): the gh
// gate check runs on an hour cadence, so a tick whose every bd gate check
// failed evaluated nothing but used to spend the hour anyway. The job must stay
// due so the next tick retries it.
func TestPatrolScanGHGatesDoesNotRecordAFailedCheck(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}
	h := &patrolScanHost{d: d, bdGateCheck: func(string, ...string) ([]byte, error) {
		return nil, errors.New("bd gate check: connection refused")
	}}

	d.patrolScanGHGates(h, []string{"myr"})

	for _, key := range []string{"patrol_scan_gh_gates", "patrol_scan_gh_gates:myr"} {
		if !evaluatePatrolDue(townRoot, key, time.Time{}, time.Now(), ghGateInterval).due {
			t.Fatalf("a tick where every gate check failed recorded %s as run; the next tick will skip the hour", key)
		}
	}
}

// Every database answering is a run.
func TestPatrolScanGHGatesRecordsARunThatAnswered(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}
	h := &patrolScanHost{d: d, bdGateCheck: func(string, ...string) ([]byte, error) {
		return []byte("No open gates of type 'gh' found.\n"), nil
	}}

	d.patrolScanGHGates(h, []string{"myr"})

	for _, key := range []string{"patrol_scan_gh_gates", "patrol_scan_gh_gates:myr"} {
		if evaluatePatrolDue(townRoot, key, time.Time{}, time.Now(), ghGateInterval).due {
			t.Fatalf("a tick whose every check answered did not record %s", key)
		}
	}
}

// bd reads one database per call, so a database whose gate check failed has
// had no gh gate evaluated even when the others answered. Recording its run
// would hold its gates for the hour (gt-kp9fo); the databases that did answer
// spend theirs, and the failure is in the log for the operator to see.
func TestPatrolScanGHGates_PartialFailure(t *testing.T) {
	t.Parallel()
	cases := []struct{ failing, failKey, answerKey string }{
		{"", "patrol_scan_gh_gates", "patrol_scan_gh_gates:myr"},
		{"myr", "patrol_scan_gh_gates:myr", "patrol_scan_gh_gates"},
	}
	for _, tc := range cases {
		t.Run("fails="+tc.failing, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			var logs strings.Builder
			d := &Daemon{logger: log.New(&logs, "", 0), config: &Config{TownRoot: townRoot}}
			h := &patrolScanHost{d: d, bdGateCheck: func(rig string, _ ...string) ([]byte, error) {
				if rig == tc.failing {
					return nil, errors.New("bd gate check: connection refused")
				}
				return []byte("No open gates of type 'gh' found.\n"), nil
			}}

			d.patrolScanGHGates(h, []string{"myr"})

			if !evaluatePatrolDue(townRoot, tc.failKey, time.Time{}, time.Now(), ghGateInterval).due {
				t.Fatal("a database whose gate check failed recorded the run; its gh gates wait the hour")
			}
			if evaluatePatrolDue(townRoot, tc.answerKey, time.Time{}, time.Now(), ghGateInterval).due {
				t.Fatal("a database whose gate check answered did not record the run")
			}
			where := gateCheckWhere(tc.failing)
			for _, want := range []string{
				"patrol_scan: " + where + ": gh gate check failed: bd gate check: connection refused",
				"patrol_scan: " + where + ": gh gates: the gate check failed; not recording the run",
			} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("the failure left no line %q in the log:\n%s", want, logs.String())
				}
			}
		})
	}
}

// A database that stays down is retried every tick, but only that database:
// the others spent their hour, and re-running their `gh` checks each two
// minutes would defeat the cadence (gt-96bsu).
func TestPatrolScanGHGates_PersistentFailureRetriesOnlyTheFailedDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}
	calls := map[string]int{}
	h := &patrolScanHost{d: d, bdGateCheck: func(rig string, _ ...string) ([]byte, error) {
		calls[rig]++
		if rig == "myr" {
			return nil, errors.New("bd gate check: connection refused")
		}
		return []byte("No open gates of type 'gh' found.\n"), nil
	}}

	for tick := 0; tick < 3; tick++ {
		d.patrolScanGHGates(h, []string{"myr", "yard"})
	}

	want := map[string]int{"": 1, "yard": 1, "myr": 3}
	for rig, n := range want {
		if calls[rig] != n {
			t.Errorf("gh gate check ran %d times over three ticks for %q, want %d (calls: %v)", calls[rig], rig, n, calls)
		}
	}
}

// The patrolscan.Env contract reads "" with no error as a molecule root that
// is gone; a root bd says is not found, or a null show, is that case and not
// an unreadable status (gt-kp9fo).
func TestPatrolScanMoleculeStatus_NotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    *wispReader
		want string
	}{
		{"not found", &wispReader{showErr: beads.ErrNotFound}, ""},
		{"null issue", &wispReader{show: map[string]*beads.Issue{"gt-wisp-x": nil}}, ""},
		{"live", &wispReader{show: map[string]*beads.Issue{"gt-wisp-x": {ID: "gt-wisp-x", Status: "hooked"}}}, "hooked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := wispHost(t, tc.r).MoleculeStatus("gt-wisp-x")
			if err != nil || got != tc.want {
				t.Fatalf("MoleculeStatus = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
	t.Run("other error stays unknown", func(t *testing.T) {
		t.Parallel()
		if _, err := wispHost(t, &wispReader{showErr: errors.New("dolt down")}).MoleculeStatus("gt-wisp-x"); err == nil {
			t.Fatal("an unreadable root reported no error")
		}
	})
}

// A bead bd says does not exist is gone, not unknown: the seat's submitted
// record outlived it and nothing is waiting to land. Any other read failure
// is unknown, and the tick leaves the seat alone on it (gt-xs1ni).
func TestPatrolScanWorkBeadTellsGoneFromUnreadable(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}
	h := &patrolScanHost{d: d}

	bd.seed("gt-live", "hooked", time.Now(), "gt:ready-to-land")
	if w, err := h.WorkBead("myr", "gt-live"); err != nil || w == nil || w.ID != "gt-live" {
		t.Fatalf("WorkBead(gt-live) = %+v, %v", w, err)
	}
	if w, err := h.WorkBead("myr", "gt-gone"); err != nil || w != nil {
		t.Fatalf("WorkBead(gt-gone) = %+v, %v; want nil, nil", w, err)
	}
	bd.showErr = errors.New("bd show: connection refused")
	if w, err := h.WorkBead("myr", "gt-live"); err == nil || w != nil {
		t.Fatalf("a failed read answered %+v, %v; want an error", w, err)
	}
}

// gt-74cbe: `open` is an assigned status, so the seat's work read must see
// it. Asking bd only for hooked and in_progress made a seat holding an
// open-assigned bead read as holding nothing, and the stall path then stopped
// and retired it while its work was still live.
func TestPatrolScanAssignedWorkReadsAnOpenAssignedBead(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}}

	bd.seed("gt-open", "open", time.Now().Add(-time.Hour))
	if w, err := h.AssignedWork("myr", "mycat"); err != nil || w == nil || w.ID != "gt-open" {
		t.Fatalf("AssignedWork over an open-assigned bead = %+v, %v; want gt-open", w, err)
	}

	// A more specific status wins when the seat holds both.
	bd.seed("gt-hooked", "hooked", time.Now().Add(-time.Minute))
	if w, err := h.AssignedWork("myr", "mycat"); err != nil || w == nil || w.ID != "gt-hooked" {
		t.Fatalf("AssignedWork over hooked and open work = %+v, %v; want gt-hooked", w, err)
	}

	// The full chain is hooked, then in_progress, then open.
	mid := newWorkBD(t)
	hMid := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: mid.open}}
	mid.seed("gt-open", "open", time.Now().Add(-time.Hour))
	mid.seed("gt-progress", "in_progress", time.Now().Add(-time.Minute))
	if w, err := hMid.AssignedWork("myr", "mycat"); err != nil || w == nil || w.ID != "gt-progress" {
		t.Fatalf("AssignedWork over in_progress and open work = %+v, %v; want gt-progress", w, err)
	}
	mid.seed("gt-hooked", "hooked", time.Now().Add(-2*time.Hour))
	if w, err := hMid.AssignedWork("myr", "mycat"); err != nil || w == nil || w.ID != "gt-hooked" {
		t.Fatalf("AssignedWork over hooked, in_progress and open work = %+v, %v; want gt-hooked", w, err)
	}

	// Finished work is not work the seat still holds.
	done := newWorkBD(t)
	done.seed("gt-closed", "closed", time.Now().Add(-time.Hour))
	hDone := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: done.open}}
	if w, err := hDone.AssignedWork("myr", "mycat"); err != nil || w != nil {
		t.Fatalf("AssignedWork over closed work = %+v, %v; want nil, nil", w, err)
	}
}

// gt-gnqmu: ActiveWork feeds the orphan and held-wisp passes, so an
// open-assigned bead must reach them, as hooked and in_progress beads already
// do. Finished work must not.
func TestPatrolScanActiveWorkReadsAnOpenAssignedBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status string
		want   bool
	}{
		{"hooked", true},
		{"in_progress", true},
		{"open", true},
		{"closed", false},
	}
	for _, tc := range cases {
		bd := newWorkBD(t)
		h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}}
		bd.seed("gt-work", tc.status, time.Now().Add(-time.Hour))

		work, err := h.ActiveWork("myr")
		if err != nil {
			t.Fatalf("ActiveWork over %s bead: %v", tc.status, err)
		}
		got := len(work) == 1 && work[0].ID == "gt-work"
		if got != tc.want {
			t.Errorf("ActiveWork over %s bead = %+v; want reached=%v", tc.status, work, tc.want)
		}
	}
}

// passEnv answers the tick's reads from the host's real ActiveWork over the
// daemon's bd fake and fixes everything else: the seat is gone by default (no
// directory, no session, no intent hold), so the recovery branch is the one
// under test. It exists because a host test on ActiveWork alone shows the
// read, not what the orphan and held-wisp passes make of it (gt-1291v).
type passEnv struct {
	host     *patrolScanHost
	wisps    []patrolscan.Wisp
	closed   []string
	reopened []string
}

func (e *passEnv) ActiveWork(rig string) ([]patrolscan.Work, error) {
	return e.host.ActiveWork(rig)
}
func (e *passEnv) Polecats(string) ([]string, error)                     { return nil, nil }
func (e *passEnv) Intent(string, string) (intent.Record, error)          { return intent.Record{}, nil }
func (e *passEnv) Assess(string, string) liveness.Result                 { return liveness.Result{} }
func (e *passEnv) AssignedWork(string, string) (*patrolscan.Work, error) { return nil, nil }
func (e *passEnv) WorkBead(string, string) (*patrolscan.Work, error)     { return nil, nil }
func (e *passEnv) ClearSubmission(string, string, string) (bool, error)  { return false, nil }
func (e *passEnv) AgentRecord(string, string) (patrolscan.AgentRecord, error) {
	return patrolscan.AgentRecord{}, nil
}
func (e *passEnv) PolecatEscalations(string) ([]patrolscan.Escalation, error) { return nil, nil }
func (e *passEnv) GitState(string, string) (patrolscan.GitState, error) {
	return patrolscan.GitState{}, nil
}
func (e *passEnv) Heartbeat(string, string) *patrolscan.Heartbeat      { return nil }
func (e *passEnv) Restart(string, string, string) error                { return nil }
func (e *passEnv) Kill(string, string, string) error                   { return nil }
func (e *passEnv) MarkIdle(string, string) (bool, error)               { return false, nil }
func (e *passEnv) ClearStuckExit(string, string, string) (bool, error) { return false, nil }
func (e *passEnv) MarkSubmitted(string, string, string) error          { return nil }
func (e *passEnv) PolecatDirExists(string, string) (bool, error)       { return false, nil }
func (e *passEnv) SessionExists(string, string) (bool, error)          { return false, nil }
func (e *passEnv) MoleculeStatus(string) (string, error)               { return "", nil }
func (e *passEnv) LeakedWisps(string) ([]patrolscan.Wisp, error)       { return e.wisps, nil }
func (e *passEnv) WispStatus(string) (string, error)                   { return "", nil }
func (e *passEnv) RootWork(string) ([]patrolscan.WorkRef, error)       { return nil, nil }
func (e *passEnv) SurvivingBranch(string, string) (string, error)      { return "", nil }
func (e *passEnv) RecordResumeBranch(string, string, string) error     { return nil }
func (e *passEnv) Comment(string, string) error                        { return nil }
func (e *passEnv) CloseMolecule(id, _ string) (int, error) {
	e.closed = append(e.closed, id)
	return 1, nil
}
func (e *passEnv) Reopen(_, id, assignee string) (bool, error) {
	e.reopened = append(e.reopened, id+"="+assignee)
	return true, nil
}

func passScanner(t *testing.T, env *passEnv, now time.Time) *patrolscan.Scanner {
	t.Helper()
	ledger := patrolscan.NewFileLedger(patrolscan.LedgerPath(t.TempDir()), 7*24*time.Hour)
	return patrolscan.New(env, ledger, patrolscan.Options{Now: func() time.Time { return now }})
}

// gt-1291v: an open-assigned bead is work the seat still holds, and the passes
// must act on it as such. Without the bead in ActiveWork the gone holder's
// work leaks uncounted and its old wisp is closed as leaked; with it, the
// orphan pass reopens the bead and the wisp stays.
func TestPatrolScanPassesTreatAnOpenAssignedBeadAsHeldWork(t *testing.T) {
	t.Parallel()
	now := time.Now()
	wisp := patrolscan.Wisp{ID: "gt-wisp-a", Status: "open", Assignee: "myr/polecats/mycat", CreatedAt: now.Add(-48 * time.Hour)}
	host := func(t *testing.T, bd *workBD) *patrolScanHost {
		t.Helper()
		return &patrolScanHost{d: &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}}
	}
	openBead := func(t *testing.T) *workBD {
		t.Helper()
		bd := newWorkBD(t)
		bd.seed("gt-open", "open", now.Add(-time.Hour))
		return bd
	}

	t.Run("orphan's open bead is reopened", func(t *testing.T) {
		t.Parallel()
		env := &passEnv{host: host(t, openBead(t))}
		r := passScanner(t, env, now).Tick("myr")
		if got := strings.Join(env.reopened, ","); got != "gt-open=myr/polecats/mycat" {
			t.Fatalf("reopened = %q, want the gone holder's open bead; report %v", got, r.Lines())
		}
		if r.Count(patrolscan.OutcomeReopened) != 1 {
			t.Fatalf("report = %v, want one reopen", r.Lines())
		}
	})

	t.Run("held wisp stays while the seat holds the open bead", func(t *testing.T) {
		t.Parallel()
		env := &passEnv{host: host(t, openBead(t)), wisps: []patrolscan.Wisp{wisp}}
		r := passScanner(t, env, now).Tick("myr")
		if len(env.closed) != 0 {
			t.Fatalf("closed %v, want the seat's wisp kept; report %v", env.closed, r.Lines())
		}
	})

	t.Run("the same wisp is closed once the seat holds nothing", func(t *testing.T) {
		t.Parallel()
		env := &passEnv{host: host(t, newWorkBD(t)), wisps: []patrolscan.Wisp{wisp}}
		r := passScanner(t, env, now).Tick("myr")
		if got := strings.Join(env.closed, ","); got != "gt-wisp-a" {
			t.Fatalf("closed = %q, want gt-wisp-a; report %v", got, r.Lines())
		}
	})
}

// ClearSubmission ends the seat's wait for the landing worker. It is the
// action half of the stale-record fix: without it the supervisor's own
// submitted guard refuses every restart (gt-xs1ni).
func TestPatrolScanClearSubmissionEndsTheWait(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: townRoot}}
	h := &patrolScanHost{d: d}
	seat := supervisor.IntentSeat(supervisor.SeatFor("myr", constants.RolePolecat, "mycat"))
	if err := intent.MarkSubmitted(townRoot, seat, "gt-work1", "gt done", time.Now()); err != nil {
		t.Fatal(err)
	}

	changed, err := h.ClearSubmission("myr", "mycat", "gt-work1")
	if err != nil || !changed {
		t.Fatalf("ClearSubmission = %v, %v; want true, nil", changed, err)
	}
	rec, err := intent.Read(townRoot, seat)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Submitted() {
		t.Fatalf("record still says submitted: %+v", rec)
	}

	// A record for another bead is left alone: the seat is waiting for
	// something else's landing.
	if err := intent.MarkSubmitted(townRoot, seat, "gt-work2", "gt done", time.Now()); err != nil {
		t.Fatal(err)
	}
	if changed, err := h.ClearSubmission("myr", "mycat", "gt-work1"); err != nil || changed {
		t.Fatalf("ClearSubmission for another bead = %v, %v; want false, nil", changed, err)
	}
}

// deadHolderBranch is the ported origin listing: the newest polecat branch
// generated for the bead, "" when origin has none, and the listing's error
// otherwise so the caller fails closed rather than releasing on a guess
// (gt-gzhin.2).
func TestPatrolScanDeadHolderBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		branches []string
		listErr  error
		want     string
		wantErr  bool
	}{
		{name: "no branch for the bead", branches: []string{"polecat/onyx/gt-other@abc"}},
		{name: "one branch", branches: []string{"polecat/onyx/gt-a@abc"}, want: "polecat/onyx/gt-a@abc"},
		{
			name: "newest generated branch wins",
			branches: []string{
				"polecat/onyx/gt-a@aaa",
				"polecat/onyx/gt-a@zzz",
			},
			want: "polecat/onyx/gt-a@zzz",
		},
		{name: "unreadable origin is an error", listErr: errors.New("ls-remote: could not read from remote"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &patrolScanHost{
				d:                  &Daemon{config: &Config{TownRoot: t.TempDir()}},
				listOriginBranches: func(string) ([]string, error) { return tc.branches, tc.listErr },
			}
			got, err := h.SurvivingBranch("myr", "gt-a")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SurvivingBranch = %q, nil; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SurvivingBranch: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SurvivingBranch = %q, want %q", got, tc.want)
			}
		})
	}
}

// The recovery writes go through one client: the resume_branch line lands in
// the bead's notes, where the spec dispatcher reads it back (gt-gzhin.3), and
// the release fires only while the dead holder still owns the bead — bd's
// --if-assignee guard is what keeps a bead re-slung to a live polecat out of
// it (gt-vm5g4).
func TestPatrolScanRecoveryWrites(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-a", Status: "hooked", Assignee: "myr/polecats/onyx"})
	h := &patrolScanHost{
		d:                 &Daemon{config: &Config{TownRoot: t.TempDir()}},
		openRecoveryBeads: func([]string) beads.Client { return db },
	}

	if err := h.RecordResumeBranch("myr", "gt-a", "polecat/onyx/gt-a@abc"); err != nil {
		t.Fatalf("RecordResumeBranch: %v", err)
	}
	reopened, err := h.Reopen("myr", "gt-a", "myr/polecats/onyx")
	if err != nil || !reopened {
		t.Fatalf("Reopen = %v, %v; want true, nil", reopened, err)
	}
	// The guard no longer holds: the bead is no longer onyx's.
	if reopened, err := h.Reopen("myr", "gt-a", "myr/polecats/onyx"); err != nil || reopened {
		t.Fatalf("Reopen with a stale guard = %v, %v; want false, nil", reopened, err)
	}

	is, err := db.Show("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	if is.Status != string(beads.StatusOpen) || is.Assignee != "" {
		t.Fatalf("bead = %s/%q, want open and unassigned", is.Status, is.Assignee)
	}
	if !strings.Contains(is.Notes, "resume_branch: polecat/onyx/gt-a@abc") {
		t.Fatalf("notes = %q", is.Notes)
	}
}

// wispReader is a workBeadReader that answers a wisp listing per status and
// records the options of every list.
type wispReader struct {
	byStatus map[string][]*beads.Issue
	listErr  map[string]error
	show     map[string]*beads.Issue
	showErr  error
	lists    []beads.ListOptions
}

func (r *wispReader) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	r.lists = append(r.lists, opts)
	if err := r.listErr[opts.Status]; err != nil {
		return nil, err
	}
	return r.byStatus[opts.Status], nil
}

func (r *wispReader) Show(id string) (*beads.Issue, error) {
	if r.showErr != nil {
		return nil, r.showErr
	}
	if is, ok := r.show[id]; ok {
		return is, nil
	}
	return nil, beads.ErrNotFound
}

func wispHost(t *testing.T, r *wispReader) *patrolScanHost {
	t.Helper()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: func([]string, time.Duration) workBeadReader { return r }}
	return &patrolScanHost{d: d}
}

// LeakedWisps asks the wisps table for every status a leak can sit in,
// pinned included, and reads each wisp's parent and creation time. One status
// that cannot be listed fails the whole read, and a creation time that does
// not parse reads as unknown rather than as very old.
func TestPatrolScanLeakedWisps(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 2, 0, 25, 24, 0, time.UTC)
	r := &wispReader{byStatus: map[string][]*beads.Issue{
		"pinned": {
			{ID: "gt-wisp-a", Status: "pinned", Assignee: "gastown/polecats/basalt", CreatedAt: created.Format(time.RFC3339)},
			{ID: "gt-wisp-b", Status: "pinned", Assignee: "gastown/polecats/basalt", Parent: "gt-wisp-root", CreatedAt: "not a time"},
		},
		"in_progress": {
			{ID: "gt-wisp-a", Status: "in_progress", Assignee: "gastown/polecats/basalt", CreatedAt: created.Format(time.RFC3339)},
			nil,
			{Status: "in_progress"},
		},
	}}
	got, err := wispHost(t, r).LeakedWisps("gastown")
	if err != nil {
		t.Fatalf("LeakedWisps: %v", err)
	}
	var statuses []string
	for _, o := range r.lists {
		if !o.Ephemeral {
			t.Errorf("list %q did not ask the wisps table", o.Status)
		}
		statuses = append(statuses, o.Status)
	}
	if want := "open,in_progress,pinned,hooked"; strings.Join(statuses, ",") != want {
		t.Errorf("statuses listed = %v, want %s", statuses, want)
	}
	if len(got) != 2 {
		t.Fatalf("wisps = %+v, want gt-wisp-a once and gt-wisp-b", got)
	}
	if got[0].ID != "gt-wisp-a" || got[0].Status != "in_progress" || !got[0].CreatedAt.Equal(created) {
		t.Errorf("first wisp = %+v", got[0])
	}
	if got[1].ID != "gt-wisp-b" || got[1].Parent != "gt-wisp-root" || !got[1].CreatedAt.IsZero() {
		t.Errorf("second wisp = %+v, want its parent and a zero creation time", got[1])
	}

	r.listErr = map[string]error{"hooked": errors.New("bd query: timeout")}
	if _, err := wispHost(t, r).LeakedWisps("gastown"); err == nil {
		t.Fatal("a status that failed to list must fail the whole read")
	}
}

// WispStatus reads the parent a leaked wisp names: a root bd says does not
// exist is gone, which is not an unknown.
func TestPatrolScanWispStatus(t *testing.T) {
	t.Parallel()
	r := &wispReader{show: map[string]*beads.Issue{"gt-wisp-root": {ID: "gt-wisp-root", Status: "open"}}}
	h := wispHost(t, r)
	if got, err := h.WispStatus("gt-wisp-root"); err != nil || got != "open" {
		t.Fatalf("WispStatus(live) = %q, %v; want open", got, err)
	}
	if got, err := h.WispStatus("gt-wisp-gone"); err != nil || got != "" {
		t.Fatalf("WispStatus(gone) = %q, %v; want empty and no error", got, err)
	}
	r.showErr = errors.New("bd show: timeout")
	if _, err := h.WispStatus("gt-wisp-root"); err == nil {
		t.Fatal("a failed read must be an error, not a gone parent")
	}
}

// RootWork reads a molecule root's blocks edges and the status bd reports for
// each work bead; parent-child edges are not work, a reaped root has none, and
// a failed read is an error rather than an empty answer.
func TestPatrolScanRootWork(t *testing.T) {
	t.Parallel()
	r := &wispReader{show: map[string]*beads.Issue{
		"gt-wisp-root": {ID: "gt-wisp-root", Status: "open", Dependencies: []beads.IssueDep{
			{ID: "gt-work", Status: "closed", DependencyType: "blocks"},
			{ID: "gt-parent", Status: "open", DependencyType: "parent-child"},
		}},
		"gt-wisp-patrol": {ID: "gt-wisp-patrol", Status: "pinned"},
	}}
	h := wispHost(t, r)
	got, err := h.RootWork("gt-wisp-root")
	if err != nil || len(got) != 1 || got[0].ID != "gt-work" || got[0].Status != "closed" {
		t.Fatalf("RootWork(root) = %+v, %v; want gt-work closed", got, err)
	}
	if got, err := h.RootWork("gt-wisp-patrol"); err != nil || len(got) != 0 {
		t.Fatalf("RootWork(patrol) = %+v, %v; want none", got, err)
	}
	if got, err := h.RootWork("gt-wisp-gone"); err != nil || len(got) != 0 {
		t.Fatalf("RootWork(gone) = %+v, %v; want none and no error", got, err)
	}
	r.showErr = errors.New("bd show: timeout")
	if _, err := h.RootWork("gt-wisp-root"); err == nil {
		t.Fatal("a failed read must be an error, not a root with no work")
	}
}

// The tick waits out a DEFERRED seat's done on the same window and the same
// event-log read as the crash detector, so the two cannot disagree on a turn
// whose bead has not closed yet (gt-iwgbd).
func TestPatrolScanDoneCloseGraceMatchesCrashDetector(t *testing.T) {
	t.Parallel()
	if got := patrolScanOptions(nil, time.Now).DoneCloseGrace; got != polecatDoneCloseGrace {
		t.Fatalf("patrol scan DoneCloseGrace = %s, crash detector polecatDoneCloseGrace = %s", got, polecatDoneCloseGrace)
	}
	const seat = "myr/polecats/mycat"
	townRoot := t.TempDir()
	line := seatEventLine(testEpoch.Add(-time.Minute), events.TypeDone, seat, "gt-xyz")
	if err := os.WriteFile(filepath.Join(townRoot, events.EventsFile), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: townRoot}, logger: discardLogger}}

	// The options the tick runs with, not the host method alone: a tick built
	// without DoneAge would never wait out a done.
	opts := h.d.patrolScanRunOptions(h)
	if opts.DoneAge == nil {
		t.Fatal("the tick's options leave DoneAge unset")
	}
	age, ok := opts.DoneAge("myr", "mycat", "gt-xyz", testEpoch)
	if !ok || age != time.Minute {
		t.Fatalf("DoneAge = %s, %v; want 1m0s, true", age, ok)
	}
	if _, ok := opts.DoneAge("myr", "mycat", "gt-other", testEpoch); ok {
		t.Fatal("DoneAge dated a done for a bead the seat did not finish")
	}
}

// The tick's Kill adapter is the supervisor's automatic-path kill, not a raw
// tmux kill: it stops the seat's live session and records the verb, and a town
// e-stop refuses it, so a stall can never outrun an operator (gt-9p99w).
func TestPatrolScanKillStopsTheSessionAndAnswersToTheEstop(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tm := newFakeTmux(newFixedClock())
	tm.addSession("myr-mycat", "claude", time.Now())
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: tm, prefixRegistryFn: myrPrefixes}
	h := &patrolScanHost{d: d}

	if err := estop.Activate(town, estop.TriggerManual, "drill"); err != nil {
		t.Fatal(err)
	}
	if err := h.Kill("myr", "mycat", "stalled seat"); !errors.Is(err, supervisor.ErrRefused) {
		t.Fatalf("Kill under the e-stop = %v, want a supervisor refusal", err)
	}
	if has, _ := tm.HasSession("myr-mycat"); !has {
		t.Fatal("the e-stop did not stop the kill")
	}
	if err := estop.Deactivate(town, false); err != nil {
		t.Fatal(err)
	}

	if err := h.Kill("myr", "mycat", "stalled seat"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if has, _ := tm.HasSession("myr-mycat"); has {
		t.Fatal("the seat's session survived the kill")
	}
	lines, err := os.ReadFile(supervisor.ActionLogPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lines), `"verb":"kill"`) {
		t.Fatalf("kill missing from the action log: %s", lines)
	}
}

// A killed sandboxed seat leaves its sandbox and registry record unless the
// kill removes them, and a seat with a record looks occupied. The sandbox is
// deleted only after the kill succeeded: a refused kill (an e-stop) leaves the
// live seat's sandbox alone, and a failed deletion is reported with the kill's
// success named.
func TestPatrolScanKillRemovesTheSandboxAfterTheKill(t *testing.T) {
	t.Parallel()
	newHost := func(t *testing.T, cleanup func(townRoot, sessionName string) error) (*patrolScanHost, *fakeTmux, string) {
		t.Helper()
		town := t.TempDir()
		tm := newFakeTmux(newFixedClock())
		tm.addSession("myr-mycat", "claude", time.Now())
		d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: tm, prefixRegistryFn: myrPrefixes}
		return &patrolScanHost{d: d, cleanupSandbox: cleanup}, tm, town
	}

	t.Run("cleans_up_the_killed_session", func(t *testing.T) {
		t.Parallel()
		var got []string
		h, tm, town := newHost(t, func(townRoot, sessionName string) error {
			got = append(got, townRoot+"|"+sessionName)
			return nil
		})
		if err := h.Kill("myr", "mycat", "stalled seat"); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if has, _ := tm.HasSession("myr-mycat"); has {
			t.Fatal("the session survived the kill")
		}
		if want := []string{town + "|myr-mycat"}; !reflect.DeepEqual(got, want) {
			t.Errorf("sandbox cleanup calls = %v, want %v", got, want)
		}
	})

	t.Run("a_refused_kill_leaves_the_sandbox", func(t *testing.T) {
		t.Parallel()
		called := false
		h, _, town := newHost(t, func(string, string) error { called = true; return nil })
		if err := estop.Activate(town, estop.TriggerManual, "drill"); err != nil {
			t.Fatal(err)
		}
		if err := h.Kill("myr", "mycat", "stalled seat"); !errors.Is(err, supervisor.ErrRefused) {
			t.Fatalf("Kill under the e-stop = %v, want a supervisor refusal", err)
		}
		if called {
			t.Error("the sandbox of a seat whose kill was refused was cleaned up")
		}
	})

	t.Run("a_failed_cleanup_is_reported", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("gateway unreachable")
		h, _, _ := newHost(t, func(string, string) error { return boom })
		err := h.Kill("myr", "mycat", "stalled seat")
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "session killed") {
			t.Errorf("Kill = %v, want the cleanup error wrapped with 'session killed'", err)
		}
	})
}
