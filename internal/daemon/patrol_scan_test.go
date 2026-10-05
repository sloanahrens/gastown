package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
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
func TestPatrolScanGitState(t *testing.T) {
	t.Parallel()
	var probed []string
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: "/town"}},
		gitState: func(path string) polecat.LiveGitState {
			probed = append(probed, path)
			if filepath.Base(path) == "clean" {
				return polecat.LiveGitState{Source: polecat.GitStateSourceLive, Branch: "polecat/clean/gt-a+x",
					Dirty: true, StashCount: 2, UnpushedCommits: 3}
			}
			return polecat.LiveGitState{Source: polecat.GitStateSourceUnknown, FailedReason: "git_state=unknown path=" + path + ": not a worktree root"}
		},
	}

	got, err := h.GitState("myr", "clean")
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	want := patrolscan.GitState{Branch: "polecat/clean/gt-a+x", Dirty: true, StashCount: 2, UnpushedCommits: 3}
	if got != want {
		t.Fatalf("GitState = %+v, want %+v", got, want)
	}
	if _, err := h.GitState("myr", "gone"); err == nil || !strings.Contains(err.Error(), "not a worktree root") {
		t.Fatalf("GitState(gone) = %v, want the probe's failure", err)
	}
	wantPaths := []string{filepath.Join("/town", "myr", "polecats", "clean"), filepath.Join("/town", "myr", "polecats", "gone")}
	if strings.Join(probed, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("probed %v, want %v", probed, wantPaths)
	}
}

// ClearEscalation is the write the tick makes on a resolved escalation: the
// agent bead goes to idle and the exit type is removed, the two fields gt
// done's exit wrote together. The guard is the record still reading as that
// exit, so a seat something else has moved on (a fresh sling, a human) is
// left to it rather than overwritten with idle.
func TestPatrolScanClearEscalation(t *testing.T) {
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

	changed, err := h.ClearEscalation("myr", "mycat")
	if err != nil || !changed {
		t.Fatalf("ClearEscalation = %v, %v; want true, nil", changed, err)
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
	if changed, err := h.ClearEscalation("myr", "mycat"); err != nil || changed {
		t.Fatalf("second ClearEscalation = %v, %v; want false, nil", changed, err)
	}
	// A seat whose state moved on is not the record the tick decided on.
	if changed, err := h.ClearEscalation("myr", "moved"); err != nil || changed {
		t.Fatalf("ClearEscalation(moved) = %v, %v; want false, nil", changed, err)
	}
	is, err = db.Show(moved)
	if err != nil {
		t.Fatal(err)
	}
	if got := beads.ParseAgentFields(is.Description).AgentState; got != "working" {
		t.Fatalf("moved seat state = %q, want working", got)
	}
	// A seat with no agent bead is a failed read, never a silent success.
	if _, err := h.ClearEscalation("myr", "absent"); err == nil {
		t.Fatal("ClearEscalation on a seat with no bead must fail")
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
