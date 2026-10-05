package patrolscan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

var errRefused = errors.New("supervisor refused: restart budget exhausted")

// fakeEnv is an in-memory Env. Unset maps answer "nothing"; the *Err fields
// make a read fail.
type fakeEnv struct {
	polecats    []string
	polecatsErr error
	intents     map[string]intent.Record
	intentErr   map[string]error
	verdicts    map[string]liveness.Result
	work        map[string]*Work
	workErr     map[string]error
	agents      map[string]AgentRecord
	stateErr    map[string]error
	heartbeats  map[string]*Heartbeat
	restartErr  map[string]error
	idleErr     map[string]error
	submitErr   map[string]error
	// gitStates is the live worktree measurement per seat; an unset seat
	// measures clean, the answer nothing-wrong. gitErr makes the probe fail.
	gitStates map[string]GitState
	gitErr    map[string]error
	// clearSeatErr makes the agent-bead reset fail.
	clearSeatErr map[string]error
	// idleStopped names seats whose record already says stop, so MarkIdle
	// reports no change.
	idleStopped map[string]bool

	active     []Work
	activeErr  error
	dirs       map[string]bool
	dirErr     map[string]error
	sessions   map[string]bool
	sessionErr map[string]error
	molStatus  map[string]string
	molErr     map[string]error
	closeErr   error
	branches   map[string]string
	branchErr  map[string]error
	commentErr error
	notesErr   map[string]error
	reopenErr  map[string]error
	// heldBeads names beads whose guarded release no longer holds: another
	// actor reassigned them since the list read.
	heldBeads map[string]bool

	restarts    []string
	idles       []string // polecats whose seat was retired to stop
	escalations []string // polecats whose agent bead was reset to idle
	submitted   []string // "polecat=bead" the tick recorded as submitted
	closed      []string
	comments    map[string][]string
	reads       []string // AgentState reads, to prove they are refusal-only
	workReads   []string // AssignedWork reads, to prove a skipped seat reads no Dolt
	recorded    []string // "bead=resume_branch: <branch>" per RecordResumeBranch
	reopened    []string // "bead=assignee" per Reopen

	// workBeads answers WorkBead by ID; a missing ID is a gone bead (nil).
	workBeads   map[string]*Work
	workBeadErr map[string]error
	cleared     []string // "rig/polecat=workBead" per ClearSubmission
	clearErr    map[string]error
}

func newFake() *fakeEnv {
	return &fakeEnv{
		intents: map[string]intent.Record{}, intentErr: map[string]error{},
		verdicts: map[string]liveness.Result{}, work: map[string]*Work{}, workErr: map[string]error{},
		agents: map[string]AgentRecord{}, stateErr: map[string]error{}, heartbeats: map[string]*Heartbeat{},
		restartErr: map[string]error{}, idleErr: map[string]error{}, submitErr: map[string]error{},
		idleStopped: map[string]bool{},
		dirs:        map[string]bool{}, dirErr: map[string]error{},
		sessions: map[string]bool{}, sessionErr: map[string]error{}, molStatus: map[string]string{},
		molErr: map[string]error{}, branches: map[string]string{}, branchErr: map[string]error{},
		notesErr: map[string]error{}, reopenErr: map[string]error{}, heldBeads: map[string]bool{},
		comments:  map[string][]string{},
		workBeads: map[string]*Work{}, workBeadErr: map[string]error{},
		clearErr:  map[string]error{},
		gitStates: map[string]GitState{}, gitErr: map[string]error{},
		clearSeatErr: map[string]error{},
	}
}

func (f *fakeEnv) Polecats(string) ([]string, error) { return f.polecats, f.polecatsErr }
func (f *fakeEnv) Intent(_, p string) (intent.Record, error) {
	return f.intents[p], f.intentErr[p]
}
func (f *fakeEnv) Assess(_, p string) liveness.Result {
	if r, ok := f.verdicts[p]; ok {
		return r
	}
	return liveness.Result{Verdict: liveness.Alive}
}
func (f *fakeEnv) AssignedWork(_, p string) (*Work, error) {
	f.workReads = append(f.workReads, p)
	return f.work[p], f.workErr[p]
}
func (f *fakeEnv) WorkBead(_, id string) (*Work, error) {
	if err := f.workBeadErr[id]; err != nil {
		return nil, err
	}
	return f.workBeads[id], nil
}
func (f *fakeEnv) ClearSubmission(rig, p, workBead string) (bool, error) {
	if err := f.clearErr[p]; err != nil {
		return false, err
	}
	f.cleared = append(f.cleared, rig+"/"+p+"="+workBead)
	return true, nil
}
func (f *fakeEnv) AgentRecord(_, p string) (AgentRecord, error) {
	f.reads = append(f.reads, p)
	return f.agents[p], f.stateErr[p]
}
func (f *fakeEnv) Heartbeat(_, p string) *Heartbeat { return f.heartbeats[p] }
func (f *fakeEnv) GitState(_, p string) (GitState, error) {
	if err := f.gitErr[p]; err != nil {
		return GitState{}, err
	}
	return f.gitStates[p], nil
}
func (f *fakeEnv) ClearEscalation(_, p string) (bool, error) {
	if err := f.clearSeatErr[p]; err != nil {
		return false, err
	}
	f.escalations = append(f.escalations, p)
	return true, nil
}
func (f *fakeEnv) Restart(_, p, _ string) error {
	if err := f.restartErr[p]; err != nil {
		return err
	}
	f.restarts = append(f.restarts, p)
	return nil
}
func (f *fakeEnv) MarkIdle(_, p string) (bool, error) {
	if err := f.idleErr[p]; err != nil {
		return false, err
	}
	f.idles = append(f.idles, p)
	if f.idleStopped[p] {
		return false, nil
	}
	f.idleStopped[p] = true
	return true, nil
}

func (f *fakeEnv) MarkSubmitted(_, p, bead string) error {
	if err := f.submitErr[p]; err != nil {
		return err
	}
	f.submitted = append(f.submitted, p+"="+bead)
	return nil
}

func (f *fakeEnv) ActiveWork(string) ([]Work, error) { return f.active, f.activeErr }
func (f *fakeEnv) PolecatDirExists(_, p string) (bool, error) {
	return f.dirs[p], f.dirErr[p]
}
func (f *fakeEnv) SessionExists(_, p string) (bool, error) {
	return f.sessions[p], f.sessionErr[p]
}
func (f *fakeEnv) MoleculeStatus(id string) (string, error) { return f.molStatus[id], f.molErr[id] }
func (f *fakeEnv) CloseMolecule(id, _ string) (int, error) {
	if f.closeErr != nil {
		return 0, f.closeErr
	}
	f.closed = append(f.closed, id)
	return 9, nil
}
func (f *fakeEnv) SurvivingBranch(_, id string) (string, error) {
	return f.branches[id], f.branchErr[id]
}
func (f *fakeEnv) RecordResumeBranch(_, id, branch string) error {
	if err := f.notesErr[id]; err != nil {
		return err
	}
	f.recorded = append(f.recorded, id+"="+ResumeBranchNote(branch))
	return nil
}
func (f *fakeEnv) Reopen(_, id, assignee string) (bool, error) {
	if err := f.reopenErr[id]; err != nil {
		return false, err
	}
	if f.heldBeads[id] {
		return false, nil
	}
	f.reopened = append(f.reopened, id+"="+assignee)
	for i := range f.active {
		if f.active[i].ID == id {
			f.active[i].Assignee = "" // released: no longer a polecat's active work
		}
	}
	return true, nil
}
func (f *fakeEnv) Comment(id, text string) error {
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments[id] = append(f.comments[id], text)
	return nil
}

type memLedger map[string]time.Time

func (m memLedger) LastReported(k string) (time.Time, bool) { t, ok := m[k]; return t, ok }
func (m memLedger) MarkReported(k string, at time.Time) error {
	m[k] = at
	return nil
}

var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func scanner(env *fakeEnv, ledger Ledger) *Scanner {
	if ledger == nil {
		ledger = memLedger{}
	}
	return New(env, ledger, Options{
		Now:       func() time.Time { return now },
		IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		HoldReason: func(w Work) string {
			if w.HasLabel("needs-human") {
				return "label needs-human"
			}
			return ""
		},
	})
}

func dead(samples int) liveness.Result {
	return liveness.Result{Verdict: liveness.Dead, Reason: liveness.ReasonNoSession,
		Sample: &intent.Progress{DeadSamples: samples}}
}

// deadWithWork sets up polecat p as dead (confirmed) holding hooked work.
func deadWithWork(env *fakeEnv, p string) {
	env.polecats = append(env.polecats, p)
	env.verdicts[p] = dead(2)
	env.work[p] = &Work{ID: "gt-" + p, Status: "hooked", Assignee: "gastown/polecats/" + p, UpdatedAt: now.Add(-time.Hour)}
	env.agents[p] = AgentRecord{State: "working"}
}

func seatFinding(t *testing.T, r Report, p string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Kind == "seat" && f.Subject == p {
			return f
		}
	}
	return Finding{}
}

func TestDeadSeatWithHookedWorkIsRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %v, want restarted", f)
	}
}

// gt done --status DEFERRED (and gt handoff's redirect) writes
// exit_type=DEFERRED and agent_state=stuck together, then retires the session:
// the bead stays hooked with the polecat's commits still in the worktree. The
// stuck bead is an exit artifact, not a polecat holding the seat, so the tick
// has to restart the seat on its preserved branch instead of skipping it
// forever while townhealth reports it dead (gt-ks62m).
func TestDeferredExitSeatIsRestartedNotSkipped(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, CleanupStatus: "has_unpushed"}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %v, want restarted", f)
	}
	if !strings.Contains(f.Detail, "DEFERRED") || !strings.Contains(f.Detail, "has_unpushed") {
		t.Fatalf("detail = %q, want the DEFERRED exit and cleanup status named", f.Detail)
	}
}

// The restart is confirmed across ticks like any other: the first dead sample
// reads as waiting, the second restarts. A skipped seat never gets there.
func TestDeferredExitSeatWaitsOutTheFirstSample(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.verdicts["ruby"] = dead(1)
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, CleanupStatus: "has_unpushed"}

	r := scanner(env, nil).Tick("gastown")

	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeWaiting {
		t.Fatalf("outcome = %v (%q), want waiting", f.Outcome, f.Detail)
	}
	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none on the first sample", env.restarts)
	}
}

// A seat the last turn left stuck after an ESCALATED exit is the operator's,
// and nothing ever clears it: gt done wrote agent_state=stuck and exit_type=
// ESCALATED and no pass reads them back, so `gt polecat list` keeps reporting
// recovery-needed on a seat with nothing at risk (gt-fn9e6.33). The case is
// beads/rust, whose escalated bead closed when the work it was raised on
// landed; here the seat is measured with everything resolved, and only then
// is the record reset.
func TestResolvedEscalationIsClearedToIdle(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.polecats = []string{"ruby"}
	env.verdicts["ruby"] = dead(2)
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitEscalated,
		LastSourceIssue: "be-bl8", CleanupStatus: "has_unpushed"}
	env.workBeads["be-bl8"] = &Work{ID: "be-bl8", Status: "closed"}
	env.gitStates["ruby"] = GitState{Branch: "polecat/rust/be-bl8+muugx4hs"}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.escalations, ","); got != "ruby" {
		t.Fatalf("cleared = %q, want ruby; report %v", got, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeCleared {
		t.Fatalf("outcome = %v (%q), want cleared", f.Outcome, f.Detail)
	}
	for _, want := range []string{"be-bl8", "closed", "no session", "worktree clean"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
	if len(env.restarts) != 0 || len(env.idles) != 0 {
		t.Fatalf("restarts = %v, idles = %v; a seat with nothing to run is neither", env.restarts, env.idles)
	}
}

// The clear is the last resort of a chain of conditions, and each one that
// fails leaves the seat exactly as the tick leaves it today: the operator's
// escalation, not the tick's. The control case proves the baseline clears, so
// a table entry that stops clearing is the condition it names.
func TestUnresolvedEscalationIsLeftAlone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(env *fakeEnv)
		outcome Outcome
	}{
		{"every condition met (control)", func(*fakeEnv) {}, OutcomeCleared},
		{"source bead still open", func(env *fakeEnv) {
			env.workBeads["gt-src"] = &Work{ID: "gt-src", Status: "in_progress"}
		}, OutcomeIdled},
		{"source bead gone, nothing proves it closed", func(env *fakeEnv) {
			delete(env.workBeads, "gt-src")
		}, OutcomeIdled},
		{"no source issue recorded", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitEscalated}
		}, OutcomeIdled},
		{"hook still set", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitEscalated,
				LastSourceIssue: "gt-src", HookBead: "gt-hook"}
		}, OutcomeIdled},
		{"session alive", func(env *fakeEnv) { env.sessions["ruby"] = true }, OutcomeIdled},
		{"worktree dirty", func(env *fakeEnv) {
			env.gitStates["ruby"] = GitState{Branch: "polecat/ruby/gt-src+x", Dirty: true}
		}, OutcomeIdled},
		{"stash present", func(env *fakeEnv) {
			env.gitStates["ruby"] = GitState{Branch: "polecat/ruby/gt-src+x", StashCount: 1}
		}, OutcomeIdled},
		{"unpushed commit", func(env *fakeEnv) {
			env.gitStates["ruby"] = GitState{Branch: "polecat/ruby/gt-src+x", UnpushedCommits: 1}
		}, OutcomeIdled},
		{"DEFERRED exit, not ESCALATED", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, LastSourceIssue: "gt-src"}
		}, OutcomeIdled},
		{"agent state not stuck", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "working", ExitType: ExitEscalated, LastSourceIssue: "gt-src"}
		}, OutcomeIdled},
		{"agent record unreadable", func(env *fakeEnv) {
			env.stateErr["ruby"] = errors.New("bd show: exit status 1")
		}, OutcomeUnknown},
		{"source bead unreadable", func(env *fakeEnv) {
			env.workBeadErr["gt-src"] = errors.New("bd show: connection refused")
		}, OutcomeUnknown},
		{"worktree unmeasurable", func(env *fakeEnv) {
			env.gitErr["ruby"] = errors.New("git state unknown: not a worktree root")
		}, OutcomeUnknown},
		{"the clear fails", func(env *fakeEnv) {
			env.clearSeatErr["ruby"] = errors.New("bd update: exit status 1")
		}, OutcomeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			env.polecats = []string{"ruby"}
			env.verdicts["ruby"] = dead(2)
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitEscalated, LastSourceIssue: "gt-src"}
			env.workBeads["gt-src"] = &Work{ID: "gt-src", Status: "closed"}
			env.gitStates["ruby"] = GitState{Branch: "polecat/ruby/gt-src+x"}
			tc.setup(env)

			r := scanner(env, nil).Tick("gastown")

			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.outcome {
				t.Fatalf("outcome = %v (%q), want %v", f.Outcome, f.Detail, tc.outcome)
			}
			wantCleared := tc.outcome == OutcomeCleared
			if got := len(env.escalations) > 0; got != wantCleared {
				t.Fatalf("cleared = %v, want %v (report %v)", got, wantCleared, r.Lines())
			}
		})
	}
}

// A seat still holding the bead it escalated on has not had its blocker
// resolved, whatever its worktree measures: the open bead is the operator's,
// and the tick leaves it exactly as it does today.
func TestEscalatedSeatHoldingItsOpenBeadIsSkipped(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitEscalated, LastSourceIssue: "gt-ruby"}
	env.workBeads["gt-ruby"] = &Work{ID: "gt-ruby", Status: "hooked"}

	r := scanner(env, nil).Tick("gastown")

	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %v (%q), want skipped", f.Outcome, f.Detail)
	}
	if len(env.escalations) != 0 || len(env.restarts) != 0 {
		t.Fatalf("cleared = %v, restarts = %v; want neither", env.escalations, env.restarts)
	}
}

// An idle polecat — session gone, no work — is not a crash, and its record
// must stop asking for a session or townhealth reports the seat dead forever
// (gt-613vw). The retirement is reported once, then the record reads as
// stopped and the next tick says nothing.
func TestIdleSeatIsRetiredToStop(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.polecats = []string{"ruby"}
	env.verdicts["ruby"] = dead(2)
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.idles, ","); got != "ruby" {
		t.Fatalf("idles = %q, want ruby; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeIdled {
		t.Fatalf("outcome = %v, want idled", f)
	}
	r = scanner(env, nil).Tick("gastown")
	if f := seatFinding(t, r, "ruby"); f.Outcome != "" {
		t.Fatalf("second tick = %v %q, want no finding", f.Outcome, f.Detail)
	}
}

// A seat whose gt done wrote gt:ready-to-land but lost its intent-record write
// is mid-landing, not dead. The tick brings the record up to the label, or the
// record says run for the whole of the landing and townhealth reports the seat
// dead (gt-2z8k1).
func TestMidLandingSeatRecordIsBroughtUpToTheLabel(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.work["ruby"].Labels = []string{"gt:ready-to-land"}
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.submitted, ","); got != "ruby=gt-ruby" {
		t.Fatalf("submitted = %q, want ruby=gt-ruby; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %v, want skipped", f)
	}
	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none", env.restarts)
	}
}

// The tick reports the failure instead of claiming the repair: the seat is
// still mid-landing, and the next tick tries again.
func TestMidLandingSeatRecordWriteFailureIsReported(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.work["ruby"].Labels = []string{"gt:ready-to-land"}
	env.submitErr["ruby"] = errors.New("intent lock: timed out")
	r := scanner(env, nil).Tick("gastown")
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v %q, want failed", f.Outcome, f.Detail)
	}
}

// A seat whose record already says submitted is left alone from the record
// check: no work read, no Dolt.
func TestSubmittedSeatIsNotReadFromDolt(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.polecats = []string{"ruby"}
	env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
	env.work["ruby"] = &Work{ID: "gt-ruby", Status: "hooked"}
	r := scanner(env, nil).Tick("gastown")
	if f := seatFinding(t, r, "ruby"); f.Outcome != "" {
		t.Fatalf("outcome = %v %q, want no finding", f.Outcome, f.Detail)
	}
	if len(env.submitted) != 0 || len(env.reads) != 0 || len(env.workReads) != 0 {
		t.Fatalf("submitted = %v, agent-state reads = %v, work reads = %v; want none",
			env.submitted, env.reads, env.workReads)
	}
}

func TestNoRestartCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(env *fakeEnv)
		outcome Outcome // "" = no finding
	}{
		{"submitted intent, bead still waiting (hazard 1, landing guard)", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
			env.workBeads["gt-ruby"] = &Work{ID: "gt-ruby", Status: "hooked", Labels: []string{"gt:ready-to-land"}}
		}, ""},
		{"submitted intent, no work_bead named (fail closed)", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted}
		}, ""},
		{"submitted intent, bead unreadable (fail closed)", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
			env.workBeadErr["gt-ruby"] = errors.New("bd show: connection refused")
		}, OutcomeUnknown},
		{"submitted intent pulled for rework, ending the wait fails", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
			env.workBeads["gt-ruby"] = &Work{ID: "gt-ruby", Status: "hooked", Labels: []string{"rework"}}
			env.clearErr["ruby"] = errors.New("intent lock: timed out")
		}, OutcomeUnknown},
		{"parked seat", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredPark, Reason: "operator"}
		}, ""},
		{"frozen seat (budget spent earlier)", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Frozen: true}
		}, ""},
		{"unreadable intent record", func(env *fakeEnv) {
			env.intentErr["ruby"] = errors.New("permission denied")
		}, OutcomeUnknown},
		{"agent_state stuck after an ESCALATED exit (the operator's)", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: "ESCALATED"}
		}, OutcomeSkipped},
		{"agent_state stuck with no exit type recorded (fail closed)", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck"}
		}, OutcomeSkipped},
		{"agent_state awaiting-gate (hazard 1)", func(env *fakeEnv) { env.agents["ruby"] = AgentRecord{State: "awaiting-gate"} }, OutcomeSkipped},
		{"agent_state paused (hazard 1)", func(env *fakeEnv) { env.agents["ruby"] = AgentRecord{State: "paused"} }, OutcomeSkipped},
		{"agent_state done", func(env *fakeEnv) { env.agents["ruby"] = AgentRecord{State: "done"} }, OutcomeSkipped},
		{"agent bead unreadable (hazard 3)", func(env *fakeEnv) {
			env.stateErr["ruby"] = errors.New("bd show: exit status 1")
		}, OutcomeUnknown},
		{"heartbeat stuck", func(env *fakeEnv) {
			env.heartbeats["ruby"] = &Heartbeat{State: "stuck", At: now.Add(-time.Hour)}
		}, OutcomeSkipped},
		{"fresh exiting heartbeat", func(env *fakeEnv) {
			env.heartbeats["ruby"] = &Heartbeat{State: "exiting", At: now.Add(-time.Minute)}
		}, OutcomeSkipped},
		{"ready-to-land work (landing worker owns it)", func(env *fakeEnv) {
			env.work["ruby"].Labels = []string{"gt:ready-to-land"}
		}, OutcomeSkipped},
		{"held work (dispatch hold rule)", func(env *fakeEnv) {
			env.work["ruby"].Labels = []string{"needs-human"}
		}, OutcomeSkipped},
		{"closed work", func(env *fakeEnv) { env.work["ruby"].Status = "closed" }, ""},
		{"no work: idle polecat retired to stop", func(env *fakeEnv) { env.work["ruby"] = nil }, OutcomeIdled},
		{"stuck after a DEFERRED exit with no hook: idle seat retired, not restarted", func(env *fakeEnv) {
			env.work["ruby"] = nil
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, CleanupStatus: "has_unpushed"}
		}, OutcomeIdled},
		{"no work, death unconfirmed", func(env *fakeEnv) {
			env.work["ruby"] = nil
			env.verdicts["ruby"] = dead(1)
		}, ""},
		{"no work, record already stopped", func(env *fakeEnv) {
			env.work["ruby"] = nil
			env.idleStopped["ruby"] = true
		}, ""},
		{"no work, retire fails", func(env *fakeEnv) {
			env.work["ruby"] = nil
			env.idleErr["ruby"] = errors.New("intent lock: timed out")
		}, OutcomeFailed},
		{"work read failure (hazard 3)", func(env *fakeEnv) {
			env.work["ruby"] = nil
			env.workErr["ruby"] = errors.New("bd list: connection refused")
		}, OutcomeUnknown},
		{"spawn grace", func(env *fakeEnv) { env.work["ruby"].UpdatedAt = now.Add(-time.Minute) }, OutcomeSkipped},
		{"first dead sample only", func(env *fakeEnv) { env.verdicts["ruby"] = dead(1) }, OutcomeWaiting},
		{"liveness unknown", func(env *fakeEnv) {
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Unknown, Err: errors.New("tmux: no server")}
		}, OutcomeUnknown},
		{"alive", func(env *fakeEnv) { env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive} }, ""},
		{"stalled is reported only", func(env *fakeEnv) {
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Stalled, Reason: "no progress for 31m"}
		}, OutcomeStalled},
		{"budget exhausted (supervisor refuses)", func(env *fakeEnv) { env.restartErr["ruby"] = errRefused }, OutcomeRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			deadWithWork(env, "ruby")
			tc.setup(env)
			r := scanner(env, nil).Tick("gastown")
			if len(env.restarts) != 0 {
				t.Fatalf("restarted %v; want no restart. report: %v", env.restarts, r.Lines())
			}
			if got := seatFinding(t, r, "ruby").Outcome; got != tc.outcome {
				t.Fatalf("outcome = %q, want %q. report: %v", got, tc.outcome, r.Lines())
			}
		})
	}
}

// gt-xs1ni: gt done submits, the landing worker (or an overseer) pulls the
// landing back for rework, and the seat's session is gone. The record still
// says submitted and gt:ready-to-land is off the bead, so the record alone
// would hold the seat forever. The tick has to read the bead, end the stale
// wait, and restart the seat on the ordinary dead-session path.
func TestSubmittedSeatWithPulledLandingIsRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
	env.workBeads["gt-ruby"] = &Work{ID: "gt-ruby", Status: "hooked", Labels: []string{"rework"}}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.cleared, ","); got != "gastown/ruby=gt-ruby" {
		t.Fatalf("cleared = %q, want the stale submission ended; report %v", got, r.Lines())
	}
	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby restarted; report %v", got, r.Lines())
	}
}

// A record naming a bead bd no longer has is stale the same way: nothing is
// waiting to land, so the seat falls through like any other.
func TestSubmittedSeatWhoseBeadIsGoneIsRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-gone"}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby restarted; report %v", got, r.Lines())
	}
}

// A landing that came back for rework and was landed again in the meantime
// leaves a closed bead: the wait is over there too, but the seat holds no
// work, so the ordinary path retires it instead of restarting it.
func TestSubmittedSeatWithClosedBeadIsNotRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
	env.workBeads["gt-ruby"] = &Work{ID: "gt-ruby", Status: "closed"}
	env.work["ruby"] = nil

	r := scanner(env, nil).Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarted %v with no work; report %v", env.restarts, r.Lines())
	}
	if len(env.cleared) != 1 {
		t.Fatalf("cleared = %v, want the stale submission ended; report %v", env.cleared, r.Lines())
	}
	if got := strings.Join(env.idles, ","); got != "ruby" {
		t.Fatalf("idles = %q, want ruby retired; report %v", got, r.Lines())
	}
}

// Held and submitted seats are decided from the intent record alone: no
// liveness sample, no bd read. A submitted record that names no work_bead
// keeps that shortcut — nothing says which bead to check, so it is taken at
// its word (gt-xs1ni).
func TestHeldSeatReadsNothingElse(t *testing.T) {
	t.Parallel()
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		env := newFake()
		deadWithWork(env, "ruby")
		env.intents["ruby"] = intent.Record{Desired: intent.DesiredPark, Reason: "operator"}
		scanner(env, nil).Tick("gastown")
		if len(env.reads) != 0 {
			t.Fatalf("agent bead read %d time(s) for a parked seat", len(env.reads))
		}
	})
	t.Run("submitted with no work_bead", func(t *testing.T) {
		t.Parallel()
		env := newFake()
		deadWithWork(env, "ruby")
		env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted}
		scanner(env, nil).Tick("gastown")
		if len(env.reads) != 0 {
			t.Fatalf("agent bead read %d time(s) for a submitted seat", len(env.reads))
		}
		if len(env.cleared) != 0 {
			t.Fatalf("cleared %v for a record that names no bead", env.cleared)
		}
	})
}

func TestPolecatListFailureIsAnErrorNotAnAllClear(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.polecatsErr = errors.New("permission denied")
	r := scanner(env, nil).Tick("gastown")
	if len(r.Errors) == 0 || r.Checked != 0 {
		t.Fatalf("want an error and nothing checked, got %+v", r)
	}
}

// goneHolder sets up bead id held by a polecat with no directory and no
// session.
func goneHolder(env *fakeEnv, id, p string) *Work {
	env.active = append(env.active, Work{ID: id, Status: "hooked", Assignee: "gastown/polecats/" + p, AttachedMolecule: "gt-wisp-" + p})
	env.molStatus["gt-wisp-"+p] = "open"
	return &env.active[len(env.active)-1]
}

func TestOrphanedBondedMoleculeIsClosed(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.closed, ","); got != "gt-wisp-onyx" {
		t.Fatalf("closed = %q, want gt-wisp-onyx; report %v", got, r.Lines())
	}
	if r.Count(OutcomeClosed) != 1 {
		t.Fatalf("report %v", r.Lines())
	}
}

func TestOrphanMoleculeGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(env *fakeEnv)
	}{
		{"holder directory exists", func(env *fakeEnv) { env.dirs["onyx"] = true }},
		{"holder session alive", func(env *fakeEnv) { env.sessions["onyx"] = true }},
		{"directory unreadable", func(env *fakeEnv) { env.dirErr["onyx"] = errors.New("EIO") }},
		{"session query failed", func(env *fakeEnv) { env.sessionErr["onyx"] = errors.New("tmux") }},
		{"molecule already closed", func(env *fakeEnv) { env.molStatus["gt-wisp-onyx"] = "closed" }},
		{"molecule reaped", func(env *fakeEnv) { env.molStatus["gt-wisp-onyx"] = "" }},
		{"molecule status unreadable", func(env *fakeEnv) { env.molErr["gt-wisp-onyx"] = errors.New("bd") }},
		{"ready-to-land", func(env *fakeEnv) { env.active[0].Labels = []string{"gt:ready-to-land"} }},
		{"other rig's polecat", func(env *fakeEnv) { env.active[0].Assignee = "beads/polecats/onyx" }},
		{"crew assignee", func(env *fakeEnv) { env.active[0].Assignee = "gastown/crew/sloan" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			goneHolder(env, "gt-a", "onyx")
			tc.setup(env)
			scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 {
				t.Fatalf("closed %v; want nothing closed", env.closed)
			}
		})
	}
}

func TestActiveWorkListFailureDoesNothing(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.activeErr = errors.New("bd list: timeout")
	r := scanner(env, nil).Tick("gastown")
	if len(env.closed)+len(env.comments) != 0 || len(r.Errors) != 1 {
		t.Fatalf("want one error and no action, got closed=%v comments=%v report=%v", env.closed, env.comments, r.Lines())
	}
}

// strandedFinding returns the recovery finding for one bead.
func strandedFinding(t *testing.T, r Report, id string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Kind == "stranded" && f.Subject == id {
			return f
		}
	}
	return Finding{}
}

// A gone holder's bead goes back to the ready queue with the branch its work
// survives on recorded, so the dispatcher resumes that branch (gt-gzhin.2).
func TestGoneHolderWithBranchIsReopened(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.reopened, ","); got != "gt-a=gastown/polecats/onyx" {
		t.Fatalf("reopened = %q, want gt-a released from onyx; report %v", got, r.Lines())
	}
	if got := strings.Join(env.recorded, ","); got != "gt-a=resume_branch: polecat/onyx/gt-a@abc" {
		t.Fatalf("recorded = %q; report %v", got, r.Lines())
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != OutcomeReopened || f.Detail != "resume_branch: polecat/onyx/gt-a@abc" {
		t.Fatalf("finding = %v %q; report %v", f.Outcome, f.Detail, r.Lines())
	}
	if !strings.Contains(strings.Join(r.Lines(), "\n"), "1 reopened") {
		t.Fatalf("summary does not count the reopen: %v", r.Lines())
	}
	c := strings.Join(env.comments["gt-a"], "\n")
	if !strings.Contains(c, "polecat/onyx/gt-a@abc") || !strings.Contains(c, "REOPENED") {
		t.Fatalf("comment = %q", c)
	}
	if len(env.restarts) != 0 {
		t.Fatalf("restarted %v; the tick reopens, it does not re-sling", env.restarts)
	}
}

// Without a branch the bead still returns to the queue, with no notes line
// claiming one survived.
func TestGoneHolderWithoutBranchIsReopened(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")

	r := scanner(env, nil).Tick("gastown")

	if len(env.recorded) != 0 {
		t.Fatalf("recorded = %v, want no resume_branch line", env.recorded)
	}
	if got := strings.Join(env.reopened, ","); got != "gt-a=gastown/polecats/onyx" {
		t.Fatalf("reopened = %q; report %v", got, r.Lines())
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != OutcomeReopened || f.Detail != "no surviving branch" {
		t.Fatalf("finding = %v %q", f.Outcome, f.Detail)
	}
	if c := strings.Join(env.comments["gt-a"], "\n"); !strings.Contains(c, "No polecat branch carries unlanded work") {
		t.Fatalf("comment = %q", c)
	}
}

// The release is guarded, so a bead another actor reassigned since the list
// read is left to that actor, silently and with no notes line written.
func TestReopenLosesTheRaceOnTheAssigneeGuard(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
	env.heldBeads["gt-a"] = true

	r := scanner(env, nil).Tick("gastown")

	if len(env.reopened) != 0 || len(env.comments) != 0 {
		t.Fatalf("reopened = %v comments = %v, want nothing", env.reopened, env.comments)
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != "" {
		t.Fatalf("finding = %v %q, want none", f.Outcome, f.Detail)
	}
}

// TestResumeBranchFromNotes pins the reader the spec dispatcher uses
// (gt-gzhin.3): the last line wins, and anything that is not the line is not
// a branch.
func TestResumeBranchFromNotes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		notes string
		want  string
	}{
		{"none", "a note\nanother note", ""},
		{"empty", "", ""},
		{"one", "lead in\n" + ResumeBranchNote("polecat/a/gt-1+mu1") + "\ntrailing", "polecat/a/gt-1+mu1"},
		{"last wins", ResumeBranchNote("polecat/a/gt-1+mu1") + "\n" + ResumeBranchNote("polecat/b/gt-1+mu2"), "polecat/b/gt-1+mu2"},
		{"blank branch ignored", ResumeBranchNote("polecat/a/gt-1+mu1") + "\n" + ResumeBranchKey, "polecat/a/gt-1+mu1"},
		{"not a line prefix", "see resume_branch: polecat/a/gt-1+mu1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResumeBranchFromNotes(tc.notes); got != tc.want {
				t.Errorf("ResumeBranchFromNotes(%q) = %q, want %q", tc.notes, got, tc.want)
			}
		})
	}
}

// A retry after a failed release does not append the line twice: the notes
// now carry it.
func TestResumeBranchLineIsNotRecordedTwice(t *testing.T) {
	t.Parallel()
	env := newFake()
	w := goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
	w.Notes = "resume_branch: polecat/onyx/gt-a@abc"

	scanner(env, nil).Tick("gastown")

	if len(env.recorded) != 0 {
		t.Fatalf("recorded = %v, want the line left alone", env.recorded)
	}
	if len(env.reopened) != 1 {
		t.Fatalf("reopened = %v, want the release retried", env.reopened)
	}
}

// The bead is released once: the next tick does not see it as a polecat's
// active work, so nothing is written a second time.
func TestReopenedBeadIsNotWrittenAgain(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
	ledger := memLedger{}
	s := scanner(env, ledger)

	s.Tick("gastown")
	s.Tick("gastown")
	if len(env.reopened) != 1 || len(env.recorded) != 1 {
		t.Fatalf("reopened = %v recorded = %v, want each once", env.reopened, env.recorded)
	}
	if n := len(env.comments["gt-a"]); n != 1 {
		t.Fatalf("comments = %d, want 1", n)
	}
}

// A live holder — directory or session — is the seat pass's business, not
// this one's.
func TestLiveHolderIsLeftAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(env *fakeEnv)
	}{
		{"holder directory exists", func(env *fakeEnv) { env.dirs["onyx"] = true }},
		{"holder session alive", func(env *fakeEnv) { env.sessions["onyx"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			goneHolder(env, "gt-a", "onyx")
			env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
			tc.setup(env)

			scanner(env, nil).Tick("gastown")

			if len(env.reopened) != 0 || len(env.comments) != 0 {
				t.Fatalf("reopened = %v comments = %v, want the bead left alone", env.reopened, env.comments)
			}
		})
	}
}

// A submitted bead belongs to the landing worker: a dead holder is expected
// there, and reopening it would take the landing back.
func TestReadyToLandHolderIsLeftAlone(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx").Labels = []string{ReadyToLandLabel}
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"

	r := scanner(env, nil).Tick("gastown")

	if len(env.reopened) != 0 || len(env.recorded) != 0 || len(env.comments) != 0 {
		t.Fatalf("reopened = %v recorded = %v comments = %v, want nothing", env.reopened, env.recorded, env.comments)
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != "" {
		t.Fatalf("finding = %v %q, want none", f.Outcome, f.Detail)
	}
}

// A parked holder is a deliberate stop: its work waits for the operator.
func TestParkedHolderIsLeftAlone(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
	env.intents["onyx"] = intent.Record{Desired: intent.DesiredPark, Reason: "operator"}

	r := scanner(env, nil).Tick("gastown")

	if len(env.reopened) != 0 || len(env.recorded) != 0 || len(env.comments) != 0 {
		t.Fatalf("reopened = %v recorded = %v comments = %v, want nothing", env.reopened, env.recorded, env.comments)
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != "" {
		t.Fatalf("finding = %v %q, want none", f.Outcome, f.Detail)
	}
}

// An unreadable seat record is a hold: the bead stays where it is.
func TestUnreadableHolderRecordLeavesTheBead(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.intentErr["onyx"] = errors.New("permission denied")

	r := scanner(env, nil).Tick("gastown")

	if len(env.reopened) != 0 || len(env.recorded) != 0 {
		t.Fatalf("acted on an unreadable holder record: %v %v", env.reopened, env.recorded)
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != OutcomeUnknown {
		t.Fatalf("finding = %v %q, want unknown", f.Outcome, f.Detail)
	}
}

// Without a survival answer the bead stays hooked: releasing on a failed
// query hands preserved work to a fresh polecat starting from main.
func TestSurvivingWorkUnknownWritesNothing(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branchErr["gt-a"] = errors.New("ls-remote timed out")
	r := scanner(env, nil).Tick("gastown")
	if len(env.comments) != 0 || len(env.reopened) != 0 || len(env.recorded) != 0 {
		t.Fatalf("acted on an unknown survival answer: reopened=%v recorded=%v comments=%v", env.reopened, env.recorded, env.comments)
	}
	found := false
	for _, f := range r.Findings {
		if f.Kind == "stranded" && f.Outcome == OutcomeUnknown {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an unknown stranded finding, got %v", r.Lines())
	}
}

// A notes write that failed leaves the bead hooked, so the next tick retries
// instead of releasing work the dispatcher would go looking for on no branch.
func TestFailedResumeBranchWriteLeavesTheBeadHooked(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
	env.notesErr["gt-a"] = errors.New("bd update: connection refused")

	r := scanner(env, nil).Tick("gastown")

	if len(env.reopened) != 0 {
		t.Fatalf("reopened = %v with the branch unrecorded", env.reopened)
	}
	if f := strandedFinding(t, r, "gt-a"); f.Outcome != OutcomeFailed {
		t.Fatalf("finding = %v %q, want failed", f.Outcome, f.Detail)
	}
}

func TestFileLedgerRoundTripAndPrune(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x", "ledger.json")
	l := NewFileLedger(path, 48*time.Hour)
	if _, ok := l.LastReported("gastown/gt-a"); ok {
		t.Fatal("empty ledger reported an entry")
	}
	if err := l.MarkReported("gastown/old", now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkReported("gastown/gt-a", now); err != nil {
		t.Fatal(err)
	}
	l2 := NewFileLedger(path, 48*time.Hour)
	if at, ok := l2.LastReported("gastown/gt-a"); !ok || !at.Equal(now) {
		t.Fatalf("reloaded = %v %v", at, ok)
	}
	if _, ok := l2.LastReported("gastown/old"); ok {
		t.Fatal("old entry not pruned")
	}
}

func TestFileLedgerUnreadableSuppressesAndRefusesOverwrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := NewFileLedger(path, time.Hour)
	if _, ok := l.LastReported("gastown/gt-a"); !ok {
		t.Fatal("an unreadable ledger must read as already reported")
	}
	if err := l.MarkReported("gastown/gt-a", now); err == nil {
		t.Fatal("overwrote an unreadable ledger")
	}
}
