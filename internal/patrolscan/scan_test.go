package patrolscan

import (
	"errors"
	"fmt"
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
	// wisps are the rig's wisps. CloseMolecule closes a listed wisp and every
	// listed wisp under it whatever their status, as beads.CloseWispTree
	// does (it skips only closed ones), and LeakedWisps lists the rest.
	wisps         []Wisp
	wispsErr      error
	wispStatus    map[string]string
	wispStatusErr map[string]error
	closeReasons  []string
	// rootWork answers RootWork per molecule root, rootWorkErr makes the read
	// fail, and rootReads records every root asked about.
	rootWork    map[string][]WorkRef
	rootWorkErr map[string]error
	rootReads   []string
	branches    map[string]string
	branchErr   map[string]error
	commentErr  error
	notesErr    map[string]error
	reopenErr   map[string]error
	// heldBeads names beads whose guarded release no longer holds: another
	// actor reassigned them since the list read.
	heldBeads map[string]bool

	restarts    []string
	kills       []string // polecats whose live session was stopped
	killErr     map[string]error
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

	// raised are the escalation records the rig's polecats raised, and
	// raisedErr makes the read fail.
	raised    []Escalation
	raisedErr error

	// seatCap is the polecat_pool cap a crash restart answers to; a zero
	// Limit is no cap. seatCapErr makes the read fail.
	seatCap    SeatCap
	seatCapErr error
	// census, when set, answers SeatCap the way the daemon does: the live
	// polecat sessions by name, counted against censusLimit with the asked
	// seat's own session left out (gt-zwiww). It replaces seatCap.
	census      map[string]bool
	censusLimit int
	capAsked    []string // "rig/name" per SeatCap read
}

// seatCapFor is the SeatCap read the scanner is given.
func (f *fakeEnv) seatCapFor(rig, name string) (SeatCap, error) {
	f.capAsked = append(f.capAsked, rig+"/"+name)
	if f.census == nil {
		return f.seatCap, f.seatCapErr
	}
	live := 0
	for p, up := range f.census {
		if up && p != name {
			live++
		}
	}
	return SeatCap{Live: live, Limit: f.censusLimit, Key: "polecat_pool.max_seats"}, nil
}

func newFake() *fakeEnv {
	return &fakeEnv{
		intents: map[string]intent.Record{}, intentErr: map[string]error{},
		verdicts: map[string]liveness.Result{}, work: map[string]*Work{}, workErr: map[string]error{},
		agents: map[string]AgentRecord{}, stateErr: map[string]error{}, heartbeats: map[string]*Heartbeat{},
		restartErr: map[string]error{}, killErr: map[string]error{}, idleErr: map[string]error{}, submitErr: map[string]error{},
		idleStopped: map[string]bool{},
		dirs:        map[string]bool{}, dirErr: map[string]error{},
		sessions: map[string]bool{}, sessionErr: map[string]error{}, molStatus: map[string]string{},
		molErr: map[string]error{}, wispStatus: map[string]string{}, wispStatusErr: map[string]error{}, branches: map[string]string{}, branchErr: map[string]error{},
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
func (f *fakeEnv) PolecatEscalations(string) ([]Escalation, error) {
	return f.raised, f.raisedErr
}
func (f *fakeEnv) GitState(_, p string) (GitState, error) {
	if err := f.gitErr[p]; err != nil {
		return GitState{}, err
	}
	return f.gitStates[p], nil
}
func (f *fakeEnv) ClearStuckExit(_, p, exit string) (bool, error) {
	if err := f.clearSeatErr[p]; err != nil {
		return false, err
	}
	f.escalations = append(f.escalations, p)
	// The write gt done's exit is undone by: state idle, exit type removed,
	// the rest of the record (source issue, cleanup status) left as it was.
	rec := f.agents[p]
	if !strings.EqualFold(rec.State, "stuck") || !strings.EqualFold(rec.ExitType, exit) {
		return false, nil
	}
	rec.State, rec.ExitType = "idle", ""
	f.agents[p] = rec
	return true, nil
}
func (f *fakeEnv) Restart(_, p, _ string) error {
	if err := f.restartErr[p]; err != nil {
		return err
	}
	f.restarts = append(f.restarts, p)
	return nil
}
func (f *fakeEnv) Kill(_, p, _ string) error {
	if err := f.killErr[p]; err != nil {
		return err
	}
	f.kills = append(f.kills, p)
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
func (f *fakeEnv) CloseMolecule(id, reason string) (int, error) {
	if f.closeErr != nil {
		return 0, f.closeErr
	}
	f.closed = append(f.closed, id)
	f.closeReasons = append(f.closeReasons, reason)
	for _, w := range f.wisps {
		if w.ID == id {
			return f.closeTree(id), nil
		}
	}
	return 9, nil
}

// closeTree closes the listed wisp id and the listed wisps under it that are
// not closed already, and returns how many it closed.
func (f *fakeEnv) closeTree(id string) int {
	n := 0
	for i := range f.wisps {
		w := &f.wisps[i]
		if w.ID == id && w.Status != "closed" {
			w.Status = "closed"
			n++
			for _, c := range f.wisps {
				if c.Parent == id {
					n += f.closeTree(c.ID)
				}
			}
		}
	}
	return n
}

func (f *fakeEnv) LeakedWisps(string) ([]Wisp, error) {
	if f.wispsErr != nil {
		return nil, f.wispsErr
	}
	var open []Wisp
	for _, w := range f.wisps {
		if w.Status != "closed" {
			open = append(open, w)
		}
	}
	return open, nil
}

func (f *fakeEnv) WispStatus(id string) (string, error) {
	return f.wispStatus[id], f.wispStatusErr[id]
}

func (f *fakeEnv) RootWork(root string) ([]WorkRef, error) {
	f.rootReads = append(f.rootReads, root)
	return f.rootWork[root], f.rootWorkErr[root]
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
		SeatCap:   env.seatCapFor,
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

// gt-02tcp: a crash restart raises a live session, so it spends a seat exactly
// as a sling does. At the pool's cap the seat stays hooked and waits, with the
// cap named in the skip line.
func TestCrashRestartWaitsAtThePoolCap(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.seatCap = SeatCap{Live: 3, Limit: 3, Key: "polecat_pool.max_seats"}

	r := scanner(env, nil).Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none while the pool is full", env.restarts)
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %v (%q), want skipped", f.Outcome, f.Detail)
	}
	for _, want := range []string{"gt-ruby", "polecat_pool.max_seats", "3/3"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
}

// The total cap (polecat_pool.max_total) holds a restart the same way, and
// names itself rather than the seat cap the operator would raise in vain.
func TestCrashRestartWaitsAtTheTotalCap(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.seatCap = SeatCap{Live: 2, Limit: 2, Key: "polecat_pool.max_total"}

	r := scanner(env, nil).Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none while the town is at its total", env.restarts)
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "polecat_pool.max_total") {
		t.Fatalf("outcome = %v (%q), want skipped naming polecat_pool.max_total", f.Outcome, f.Detail)
	}
}

// The wait is not a decision against the seat: it is deferred to the first
// tick that finds a live seat freed, and it does not reset the dead samples
// the restart was already waiting on — the seat restarts on the very next scan
// once the cap has room.
func TestCrashRestartGoesAheadOnceASeatIsFree(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.seatCap = SeatCap{Live: 3, Limit: 3, Key: "polecat_pool.max_seats"}

	if r := scanner(env, nil).Tick("gastown"); len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none while the pool is full; report %v", env.restarts, r.Lines())
	}

	// A live polecat's session ended: the census drops one and the restart
	// goes ahead on the next scan.
	env.seatCap = SeatCap{Live: 2, Limit: 3, Key: "polecat_pool.max_seats"}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby once a seat is free; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
	}
}

// gt-zwiww: a polecat whose agent died with its tmux session kept (liveness
// ReasonAgentGone) is restarted in that session, one for one. At max_seats=1
// with that session the only live polecat, the seat must restart: its own
// session is not a seat the restart has to wait for.
func TestAgentGoneSeatRestartsAtACapItsOwnSessionFills(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Dead, Reason: liveness.ReasonAgentGone,
		Sample: &intent.Progress{DeadSamples: 2}}
	env.census, env.censusLimit = map[string]bool{"ruby": true}, 1

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
	}
	if got := strings.Join(env.capAsked, ","); got != "gastown/ruby" {
		t.Fatalf("seat cap asked for %q, want the seat being restarted", got)
	}
}

// The exclusion is the seat's own session and nothing more: with the cap full
// of other live seats, a dead seat whose session is gone would add one, so it
// still waits for a free seat.
func TestSessionGoneSeatStillWaitsAtACapFullOfOthers(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.census, env.censusLimit = map[string]bool{"onyx": true}, 1

	r := scanner(env, nil).Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none while other seats fill the cap", env.restarts)
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "1/1") {
		t.Fatalf("outcome = %v (%q), want skipped at 1/1", f.Outcome, f.Detail)
	}
}

// Under the cap nothing moved: the two-dead-sample rule still gates the
// restart, so the first sample waits and the second restarts exactly as it did
// before gt-02tcp, and an uncapped pool is never full.
func TestCrashRestartUnderTheCapKeepsItsTiming(t *testing.T) {
	t.Parallel()
	const key = "polecat_pool.max_seats"
	cases := []struct {
		name    string
		cap     SeatCap
		samples int
		restart bool
	}{
		{"under the seat cap, first sample", SeatCap{Live: 1, Limit: 3, Key: key}, 1, false},
		{"under the seat cap, second sample", SeatCap{Live: 1, Limit: 3, Key: key}, 2, true},
		{"at the cap less one, second sample", SeatCap{Live: 2, Limit: 3, Key: key}, 2, true},
		{"no cap read at all", SeatCap{}, 2, true},
		{"a written zero is no cap", SeatCap{Live: 5, Limit: 0, Key: key}, 2, true},
		// gt-h6423: cap 0 closes the seat, so it holds the restart where a
		// written zero in Limit alone is no cap.
		{"a seat the operator closed", SeatCap{Live: 1, Closed: true, Key: "polecat_pool.seats.deepseek-flash.cap"}, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			deadWithWork(env, "ruby")
			env.verdicts["ruby"] = dead(tc.samples)
			env.seatCap = tc.cap

			scanner(env, nil).Tick("gastown")

			if got := len(env.restarts) == 1; got != tc.restart {
				t.Fatalf("restarted = %v, want %v (restarts %v)", got, tc.restart, env.restarts)
			}
		})
	}
}

// A seat inside its closed window is held like a closed cap (gt-her01), and
// the skip line names the window and its opening hour rather than claiming a
// cap of 0 the seat does not have.
func TestClosedWindowSeatNamesItsWindow(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.seatCap = SeatCap{
		Live:         1,
		Closed:       true,
		Key:          "polecat_pool.seats.deepseek-flash.closed",
		ClosedDetail: "the seat is closed until 16:00 (polecat_pool.seats.deepseek-flash.closed)",
	}

	r := scanner(env, nil).Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none on a closed window", env.restarts)
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "closed until 16:00") {
		t.Fatalf("outcome = %v (%q), want skipped naming the window", f.Outcome, f.Detail)
	}
	if strings.Contains(f.Detail, "is 0") {
		t.Fatalf("detail = %q, want no claim of a cap of 0", f.Detail)
	}
}

// A cap that cannot be read is an unknown, not a full pool and not a free one:
// a restart taken on an unread cap is a session raised past a limit nobody
// measured, so the seat waits and the tick says why.
func TestUnreadableSeatCapLeavesTheSeatHooked(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.seatCapErr = errors.New("reading town settings: permission denied")

	r := scanner(env, nil).Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none on an unread cap", env.restarts)
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeUnknown || !strings.Contains(f.Detail, "seat cap unreadable") {
		t.Fatalf("outcome = %v (%q), want unknown naming the seat cap", f.Outcome, f.Detail)
	}
}

// A DEFERRED or ESCALATED turn's restart is a recovery of work the town
// already holds, not a crash relaunch, and keeps the handling its own bead
// gave it: the cap that holds a crashed seat does not hold these (gt-02tcp
// acceptance).
func TestRecoveryRestartsAreNotHeldByThePoolCap(t *testing.T) {
	t.Parallel()
	t.Run("DEFERRED", func(t *testing.T) {
		t.Parallel()
		env := newFake()
		deadWithWork(env, "ruby")
		env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, CleanupStatus: "has_unpushed"}
		env.seatCap = SeatCap{Live: 3, Limit: 3, Key: "polecat_pool.max_seats"}

		r := scanner(env, nil).Tick("gastown")

		if got := strings.Join(env.restarts, ","); got != "ruby" {
			t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
		}
		if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
			t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
		}
	})
	t.Run("ESCALATED", func(t *testing.T) {
		t.Parallel()
		env := newFake()
		stoppedOnEscalation(env, "ruby")
		env.raised = []Escalation{{
			ID: "hq-wisp-36g1ez", EscalatedBy: "gastown/polecats/ruby", Title: "gt-ruby: a decision",
		}}
		env.seatCap = SeatCap{Live: 3, Limit: 3, Key: "polecat_pool.max_seats"}

		r := scanner(env, nil).Tick("gastown")

		if got := strings.Join(env.restarts, ","); got != "ruby" {
			t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
		}
		if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
			t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
		}
	})
}

// gt-6hby3: a working polecat keeps writing its own work bead, so the bead is
// recent whenever the session dies — reading that write as a dispatch held a
// dead seat for a fresh grace window on every tick (beads/guzzle waited out a
// restart that way). The grace belongs to the agent record's state: only a
// seat that still says spawning is starting up.
func TestSpawnGraceTracksTheAgentStateNotTheWorkBeadWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		state    string
		workAge  time.Duration
		agentAge time.Duration
		want     Outcome
	}{
		{"working seat with a bead written 30s ago is a crash, not a dispatch", "working", 30 * time.Second, 30 * time.Second, OutcomeRestarted},
		{"spawning seat inside the window may be starting up", "spawning", 30 * time.Second, 30 * time.Second, OutcomeSkipped},
		{"spawning seat older than the window never came up", "spawning", 10 * time.Minute, 10 * time.Minute, OutcomeRestarted},
		// gt-i3ky2: nothing moves the record off spawning once the session is
		// up, so a polecat that has run for 15 minutes and just wrote its
		// work bead still says spawning. The record's own age is the spawn
		// time, and it has outlived the window.
		{"spawning record from 15 minutes ago with a work bead written 30s ago is a crash", "spawning", 30 * time.Second, 15 * time.Minute, OutcomeRestarted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			deadWithWork(env, "ruby")
			env.agents["ruby"] = AgentRecord{State: tc.state, UpdatedAt: now.Add(-tc.agentAge)}
			env.work["ruby"].UpdatedAt = now.Add(-tc.workAge)

			r := scanner(env, nil).Tick("gastown")

			if got := seatFinding(t, r, "ruby").Outcome; got != tc.want {
				t.Fatalf("outcome = %v, want %v; report %v", got, tc.want, r.Lines())
			}
		})
	}
}

// SpawnGrace is the rule the tick and the daemon's crash detector both apply
// (TestPatrolScanSpawnGraceMatchesPolecat pins the two copies): only a
// spawning state earns the window, and every fact it is missing returns
// false, so the seat falls through to the crash path.
func TestSpawnGrace(t *testing.T) {
	t.Parallel()
	grace := 5 * time.Minute
	cases := []struct {
		name       string
		agentState string
		updatedAt  time.Time
		now        time.Time
		grace      time.Duration
		want       bool
	}{
		{"spawning inside the window", "spawning", now.Add(-30 * time.Second), now, grace, true},
		{"spawning at the window edge", "spawning", now.Add(-grace), now, grace, false},
		{"spawning past the window", "spawning", now.Add(-10 * time.Minute), now, grace, false},
		{"working", "working", now.Add(-30 * time.Second), now, grace, false},
		{"idle", "idle", now.Add(-30 * time.Second), now, grace, false},
		{"unknown state", "patrolling", now.Add(-30 * time.Second), now, grace, false},
		{"no state recorded", "", now.Add(-30 * time.Second), now, grace, false},
		{"no bead timestamp", "spawning", time.Time{}, now, grace, false},
		{"no clock", "spawning", now.Add(-30 * time.Second), time.Time{}, grace, false},
		{"no window", "spawning", now.Add(-30 * time.Second), now, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SpawnGrace(tc.agentState, tc.updatedAt, tc.now, tc.grace); got != tc.want {
				t.Fatalf("SpawnGrace(%q, %s, %s, %s) = %v, want %v",
					tc.agentState, tc.updatedAt, tc.now, tc.grace, got, tc.want)
			}
		})
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

// A report-only run that ran gt done for its bead without closing it is a
// finished turn, not a crash: the restart waits for the close to arrive, and
// goes ahead once the grace has passed with the bead still hooked (gt-iwgbd).
func TestDeferredExitSeatWaitsOutTheDoneCloseGrace(t *testing.T) {
	t.Parallel()
	const grace = 5 * time.Minute
	env := newFake()
	deadWithWork(env, "ruby")
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, CleanupStatus: "clean"}

	clock := now
	doneAt := now.Add(-time.Minute)
	var asked []string
	s := New(env, memLedger{}, Options{
		Now:            func() time.Time { return clock },
		SeatCap:        env.seatCapFor,
		DoneCloseGrace: grace,
		DoneAge: func(_, name, bead string, at time.Time) (time.Duration, bool) {
			asked = append(asked, name+" "+bead)
			if bead != "gt-ruby" {
				return 0, false
			}
			return at.Sub(doneAt), true
		},
	})

	r := s.Tick("gastown")
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "gt done") {
		t.Fatalf("inside the grace: %v, want skipped on the done", f)
	}
	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none inside the grace", env.restarts)
	}

	clock = doneAt.Add(grace + time.Second)
	r = s.Tick("gastown")
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
		t.Fatalf("past the grace: %v, want restarted", f)
	}
	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby once the grace passed", got)
	}
	if want := []string{"ruby gt-ruby", "ruby gt-ruby"}; strings.Join(asked, "|") != strings.Join(want, "|") {
		t.Fatalf("event log asked %q, want one read per tick for the hooked bead", asked)
	}
}

// Only a done for the bead the seat still holds buys the wait, and only a
// DEFERRED seat reads the event log at all: a seat with no done event restarts
// as it always did, and a crash restart never asks (gt-iwgbd).
func TestDeferredExitSeatWithoutADoneRestartsAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		agent     AgentRecord
		wantReads int
	}{
		{"DEFERRED exit, no done event for the bead", AgentRecord{State: "stuck", ExitType: ExitDeferred}, 1},
		{"crash, not a DEFERRED exit", AgentRecord{State: "working"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			deadWithWork(env, "ruby")
			env.agents["ruby"] = tc.agent
			reads := 0
			s := New(env, memLedger{}, Options{
				Now:            func() time.Time { return now },
				SeatCap:        env.seatCapFor,
				DoneCloseGrace: 5 * time.Minute,
				DoneAge: func(_, _, _ string, _ time.Time) (time.Duration, bool) {
					reads++
					return 0, false
				},
			})

			r := s.Tick("gastown")

			if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
				t.Fatalf("outcome = %v, want restarted", f)
			}
			if reads != tc.wantReads {
				t.Fatalf("event log reads = %d, want %d", reads, tc.wantReads)
			}
		})
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
		{"DEFERRED exit, its bead closed (report-only)", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, LastSourceIssue: "gt-src"}
		}, OutcomeCleared},
		{"DEFERRED exit, source bead still open", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, LastSourceIssue: "gt-src"}
			env.workBeads["gt-src"] = &Work{ID: "gt-src", Status: "in_progress"}
		}, OutcomeIdled},
		{"DEFERRED exit, commits left unpushed", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, LastSourceIssue: "gt-src"}
			env.gitStates["ruby"] = GitState{Branch: "polecat/ruby/gt-src+x", UnpushedCommits: 2}
		}, OutcomeIdled},
		{"exit type neither ESCALATED nor DEFERRED", func(env *fakeEnv) {
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: "COMPLETED", LastSourceIssue: "gt-src"}
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

// A report-only job (mol-deep-review) closes its bead and then exits gt done
// --status DEFERRED, which writes agent_state=stuck and exit_type=DEFERRED. The
// seat has no work, so the tick used to retire only its intent record to stop
// and leave the agent bead stuck, which `gt polecat list` kept reporting as
// recovery-needed until the seat was nuked by hand (gt-oa6du, dementus on
// gt-yx415, 2026-10-11). The first tick clears the record to idle; the next
// retires the seat, and the agent bead reads idle throughout.
func TestFinishedReportOnlyDeferredExitReturnsToIdle(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.polecats = []string{"dementus"}
	env.verdicts["dementus"] = dead(2)
	env.agents["dementus"] = AgentRecord{State: "stuck", ExitType: ExitDeferred,
		LastSourceIssue: "gt-yx415", CleanupStatus: "clean"}
	env.workBeads["gt-yx415"] = &Work{ID: "gt-yx415", Status: "closed"}
	env.gitStates["dementus"] = GitState{Branch: "polecat/dementus/gt-yx415+x"}
	sc := scanner(env, nil)

	r := sc.Tick("gastown")
	f := seatFinding(t, r, "dementus")
	if f.Outcome != OutcomeCleared {
		t.Fatalf("first tick = %v (%q), want cleared", f.Outcome, f.Detail)
	}
	for _, want := range []string{"DEFERRED", "gt-yx415", "closed", "worktree clean"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
	if got := env.agents["dementus"]; got.State != "idle" || got.ExitType != "" {
		t.Fatalf("agent record = %+v, want idle with no exit type", got)
	}

	r = sc.Tick("gastown")
	f = seatFinding(t, r, "dementus")
	if f.Outcome != OutcomeIdled {
		t.Fatalf("second tick = %v (%q), want idled", f.Outcome, f.Detail)
	}
	if got := env.agents["dementus"]; got.State != "idle" {
		t.Fatalf("agent record = %+v after the idle retire, want it still idle", got)
	}
	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v; a finished report-only seat is never restarted", env.restarts)
	}
}

// The clear must not touch a DEFERRED exit that still has work behind it: the
// bead is hooked, so the seat is restarted on its preserved branch as before
// (gt-ks62m) and its record stays stuck until the successor's turn starts.
func TestDeferredExitWithHookedBeadIsStillRestartedNotCleared(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred,
		LastSourceIssue: "gt-ruby", CleanupStatus: "has_unpushed"}
	env.workBeads["gt-ruby"] = &Work{ID: "gt-ruby", Status: "closed"} // a stale source must not matter
	env.gitStates["ruby"] = GitState{Branch: "polecat/ruby/gt-ruby+x", UnpushedCommits: 1}

	r := scanner(env, nil).Tick("gastown")

	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
	}
	if len(env.escalations) != 0 {
		t.Fatalf("cleared = %v, want nothing cleared on a seat with work hooked", env.escalations)
	}
}

// stoppedOnEscalation sets up polecat p as dead holding its hooked bead, with
// the agent record gt done's ESCALATED exit leaves behind: the polecat stopped
// on purpose to raise a blocker for a human.
func stoppedOnEscalation(env *fakeEnv, p string) {
	deadWithWork(env, p)
	env.agents[p] = AgentRecord{State: "stuck", ExitType: ExitEscalated, CleanupStatus: "has_unpushed"}
}

// A polecat that stopped on an escalation (gt done --status ESCALATED) is the
// operator's while the escalation it raised for its bead stands, and is
// otherwise stuck forever: nothing restarts it, and its work bead is still
// held so the no-work path never sees it. Measured on beads/guzzle 2026-10-05:
// the operator answered the decision and closed hq-wisp-36g1ez, and the seat
// sat skipped on every tick for 14 hours (gt-4hduq). Once the escalation that
// turn raised for the bead is closed, the seat is resumed on its preserved
// branch like a DEFERRED exit's.
func TestEscalatedSeatIsReleasedByItsClosedEscalation(t *testing.T) {
	t.Parallel()
	env := newFake()
	stoppedOnEscalation(env, "ruby")
	env.raised = []Escalation{{
		ID: "hq-wisp-36g1ez", EscalatedBy: "gastown/polecats/ruby",
		Title: "gt-ruby: a CI-tier decision the bead puts out of scope", Open: false,
	}}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
	}
	for _, want := range []string{"ESCALATED", "hq-wisp-36g1ez", "closed"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
}

// The release is the last step of a chain of conditions, and each one that
// fails leaves the seat exactly as the tick leaves it today: the operator's
// escalation, not the tick's, so a seat still holding the bead it escalated on
// stays skipped whatever its worktree measures. The control case proves the
// baseline releases, so a table entry that stops releasing is the condition it
// names.
func TestEscalatedSeatHoldsWhileItsDecisionStands(t *testing.T) {
	t.Parallel()
	const seat = "gastown/polecats/ruby"
	cases := []struct {
		name    string
		raised  []Escalation
		err     error
		outcome Outcome
	}{
		{"closed escalation the seat raised for the bead (control)",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: seat, Title: "gt-ruby: a decision"}}, nil, OutcomeRestarted},
		{"still open",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: seat, Title: "gt-ruby: a decision", Open: true}}, nil, OutcomeSkipped},
		{"open beside a closed one for the same bead",
			[]Escalation{
				{ID: "hq-wisp-1", EscalatedBy: seat, Title: "gt-ruby: a decision"},
				{ID: "hq-wisp-2", EscalatedBy: seat, Title: "gt-ruby: the same decision", Open: true},
			}, nil, OutcomeSkipped},
		{"named by related_bead rather than the title",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: seat, Title: "a decision", RelatedBead: "gt-ruby"}}, nil, OutcomeRestarted},
		{"escalation names another bead",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: seat, Title: "gt-other: a decision"}}, nil, OutcomeSkipped},
		{"the bead only mentioned in the title, not named",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: seat, Title: "escalating about gt-ruby"}}, nil, OutcomeSkipped},
		{"another role's escalation for the bead proves nothing about the seat",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: "gastown/crew/sloan", Title: "gt-ruby: a decision"}}, nil, OutcomeSkipped},
		{"another seat's escalation names the bead",
			[]Escalation{{ID: "hq-wisp-1", EscalatedBy: "gastown/polecats/pearl", Title: "gt-ruby: a decision"}}, nil, OutcomeSkipped},
		{"no escalation found", nil, nil, OutcomeSkipped},
		{"the escalation read fails",
			nil, errors.New("bd list escalations: connection refused"), OutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			stoppedOnEscalation(env, "ruby")
			env.raised, env.raisedErr = tc.raised, tc.err

			r := scanner(env, nil).Tick("gastown")

			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.outcome {
				t.Fatalf("outcome = %v (%q), want %v", f.Outcome, f.Detail, tc.outcome)
			}
			wantRestart := tc.outcome == OutcomeRestarted
			if got := len(env.restarts) > 0; got != wantRestart {
				t.Fatalf("restarted = %v, want %v (report %v)", got, wantRestart, r.Lines())
			}
			// A held seat is left alone entirely: no agent-bead reset, no idle
			// retirement beside a restart the tick did not make. The escalation
			// rule only ever adds a release; it never takes a seat the
			// operator's escalation still holds.
			if !wantRestart && (len(env.escalations) != 0 || len(env.idles) != 0) {
				t.Fatalf("cleared = %v, idles = %v; want nothing done", env.escalations, env.idles)
			}
		})
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
		{"spawn grace (agent_state spawning, hook inside the window)", func(env *fakeEnv) {
			env.work["ruby"].UpdatedAt = now.Add(-time.Minute)
			env.agents["ruby"] = AgentRecord{State: SpawningAgentState, UpdatedAt: now.Add(-time.Minute)}
		}, OutcomeSkipped},
		{"first dead sample only", func(env *fakeEnv) { env.verdicts["ruby"] = dead(1) }, OutcomeWaiting},
		{"liveness unknown", func(env *fakeEnv) {
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Unknown, Err: errors.New("tmux: no server")}
		}, OutcomeUnknown},
		{"alive", func(env *fakeEnv) { env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive} }, ""},
		{"stalled is reported only", func(env *fakeEnv) {
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Stalled, Reason: "no progress for 31m", QuietFor: 31 * time.Minute}
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

// gt-9p99w: a live session stalled on a work bead that is already closed (or
// on no work at all) has nothing left to progress on. It never stops by
// itself and keeps counting toward its seat cap, so the tick stops the session
// and retires the record, as the dead-session path retires an idle seat. A
// stalled seat whose work is still in play, or whose work cannot be read, is
// left exactly as it was.
func TestStalledSeatOnFinishedWorkIsReleased(t *testing.T) {
	t.Parallel()
	stalled := liveness.Result{Verdict: liveness.Stalled, Reason: "no progress for 31m", QuietFor: 31 * time.Minute}
	cases := []struct {
		name       string
		setup      func(*fakeEnv)
		want       Outcome
		wantKilled bool
		wantIdled  bool
	}{
		{"closed work", func(env *fakeEnv) { env.work["ruby"].Status = "closed" }, OutcomeReleased, true, true},
		{"tombstoned work", func(env *fakeEnv) { env.work["ruby"].Status = "tombstone" }, OutcomeReleased, true, true},
		{"no work", func(env *fakeEnv) { env.work["ruby"] = nil }, OutcomeReleased, true, true},
		{"open work", func(*fakeEnv) {}, OutcomeStalled, false, false},
		{"in_progress work", func(env *fakeEnv) { env.work["ruby"].Status = "in_progress" }, OutcomeStalled, false, false},
		{"submitted work", func(env *fakeEnv) {
			env.work["ruby"].Labels = []string{ReadyToLandLabel}
		}, OutcomeStalled, false, false},
		{"held work", func(env *fakeEnv) { env.work["ruby"].Labels = []string{"needs-human"} }, OutcomeStalled, false, false},
		{"work read fails", func(env *fakeEnv) {
			env.workErr["ruby"] = errors.New("bd list: connection refused")
		}, OutcomeUnknown, false, false},
		{"the supervisor refuses the kill", func(env *fakeEnv) {
			env.work["ruby"].Status = "closed"
			env.killErr["ruby"] = errRefused
		}, OutcomeRefused, false, true},
		{"the kill fails", func(env *fakeEnv) {
			env.work["ruby"].Status = "closed"
			env.killErr["ruby"] = errors.New("tmux: no server")
		}, OutcomeFailed, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			env.polecats = []string{"ruby"}
			env.verdicts["ruby"] = stalled
			env.work["ruby"] = &Work{ID: "gt-ruby", Status: "hooked", Assignee: "gastown/polecats/ruby", UpdatedAt: now.Add(-time.Hour)}
			env.agents["ruby"] = AgentRecord{State: "working"}
			tc.setup(env)
			r := scanner(env, nil).Tick("gastown")
			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q. report: %v", f.Outcome, tc.want, r.Lines())
			}
			if got := len(env.kills) > 0; got != tc.wantKilled {
				t.Fatalf("killed = %v, want %v; kills %v. report: %v", got, tc.wantKilled, env.kills, r.Lines())
			}
			if got := len(env.idles) > 0; got != tc.wantIdled {
				t.Fatalf("retired to stop = %v, want %v. report: %v", got, tc.wantIdled, r.Lines())
			}
			if len(env.restarts) != 0 {
				t.Fatalf("restarted %v; a stall never deserves a fresh session. report: %v", env.restarts, r.Lines())
			}
			if tc.wantKilled && len(env.kills) != 1 {
				t.Fatalf("kills = %v, want exactly one stop", env.kills)
			}
			if tc.want == OutcomeReleased {
				if !strings.Contains(f.Detail, "31m0s") {
					t.Fatalf("detail = %q, want the quiet time", f.Detail)
				}
				if env.work["ruby"] != nil && !strings.Contains(f.Detail, "gt-ruby is") {
					t.Fatalf("detail = %q, want the bead", f.Detail)
				}
			}
		})
	}
}

// A stall that does not measure past the threshold is reported, not acted on:
// the verdict and the quiet it measured have to agree before a session dies.
func TestStalledSeatBelowTheThresholdIsReportOnly(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.polecats = []string{"ruby"}
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Stalled, Reason: "no progress for 5m", QuietFor: 5 * time.Minute}
	env.work["ruby"] = &Work{ID: "gt-ruby", Status: "closed", Assignee: "gastown/polecats/ruby"}

	r := scanner(env, nil).Tick("gastown")

	if got := seatFinding(t, r, "ruby").Outcome; got != OutcomeStalled {
		t.Fatalf("outcome = %q, want %q. report: %v", got, OutcomeStalled, r.Lines())
	}
	if len(env.kills) != 0 || len(env.idles) != 0 {
		t.Fatalf("killed %v and retired %v; want neither. report: %v", env.kills, env.idles, r.Lines())
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

// leakedWisp is a wisp held by polecat p, created age ago.
func leakedWisp(id, p, status string, age time.Duration) Wisp {
	return Wisp{ID: id, Status: status, Assignee: "gastown/polecats/" + p, CreatedAt: now.Add(-age)}
}

// stepUnderRoot is a wisp held by polecat p, hanging under the root gt-root.
func stepUnderRoot(id, p, status string, age time.Duration) Wisp {
	w := leakedWisp(id, p, status, age)
	w.Parent = "gt-root"
	return w
}

func wispClosed(env *fakeEnv, id string) bool {
	for _, w := range env.wisps {
		if w.ID == id {
			return w.Status == "closed"
		}
	}
	return false
}

func TestLeakedWispsOfAGoneSeatAreClosed(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.wisps = []Wisp{
		leakedWisp("gt-wisp-a", "onyx", "pinned", 10*24*time.Hour),
		leakedWisp("gt-wisp-b", "onyx", "in_progress", 9*24*time.Hour),
		leakedWisp("gt-wisp-c", "onyx", "open", 25*time.Hour),
		leakedWisp("gt-wisp-d", "onyx", "hooked", 25*time.Hour),
	}
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.closed, ","); got != "gt-wisp-a,gt-wisp-b,gt-wisp-c,gt-wisp-d" {
		t.Fatalf("closed = %q; report %v", got, r.Lines())
	}
	for _, id := range []string{"gt-wisp-a", "gt-wisp-b", "gt-wisp-c", "gt-wisp-d"} {
		if !wispClosed(env, id) {
			t.Errorf("%s is still not closed", id)
		}
	}
	for _, reason := range env.closeReasons {
		if want := "Leaked wisp: holder onyx no longer exists (patrol scan)"; reason != want {
			t.Errorf("reason = %q, want %q", reason, want)
		}
	}
	if r.Count(OutcomeClosed) != 4 {
		t.Fatalf("report %v", r.Lines())
	}
}

func TestLeakedWispGuards(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name        string
		setup       func(env *fakeEnv)
		wantUnknown bool
	}{
		{"holder directory exists", func(env *fakeEnv) { env.dirs["onyx"] = true }, false},
		{"holder session alive", func(env *fakeEnv) { env.sessions["onyx"] = true }, false},
		{"directory unreadable", func(env *fakeEnv) { env.dirErr["onyx"] = boom }, true},
		{"session query failed", func(env *fakeEnv) { env.sessionErr["onyx"] = boom }, true},
		{"intent record unreadable", func(env *fakeEnv) { env.intentErr["onyx"] = boom }, true},
		{"seat parked", func(env *fakeEnv) { env.intents["onyx"] = intent.Record{Desired: intent.DesiredPark} }, false},
		{"seat frozen", func(env *fakeEnv) { env.intents["onyx"] = intent.Record{Frozen: true} }, false},
		// recover owns a gone seat's work bead; here it cannot release it, so
		// the seat still holds work when the wisp pass reads.
		{"seat holds a hooked bead", func(env *fakeEnv) {
			env.active = []Work{{ID: "gt-a", Status: "hooked", Assignee: "gastown/polecats/onyx"}}
			env.heldBeads["gt-a"] = true
		}, false},
		{"seat holds an in_progress bead", func(env *fakeEnv) {
			env.active = []Work{{ID: "gt-a", Status: "in_progress", Assignee: "gastown/polecats/onyx"}}
			env.heldBeads["gt-a"] = true
		}, false},
		{"active work unreadable", func(env *fakeEnv) { env.activeErr = boom }, true},
		{"wisp listing failed", func(env *fakeEnv) { env.wispsErr = boom }, true},
		{"24 hours old exactly", func(env *fakeEnv) { env.wisps[0].CreatedAt = now.Add(-24 * time.Hour) }, false},
		{"age unknown", func(env *fakeEnv) { env.wisps[0].CreatedAt = time.Time{} }, true},
		{"no assignee", func(env *fakeEnv) { env.wisps[0].Assignee = "" }, false},
		{"crew assignee", func(env *fakeEnv) { env.wisps[0].Assignee = "gastown/crew/sloan" }, false},
		{"other rig's polecat", func(env *fakeEnv) { env.wisps[0].Assignee = "beads/polecats/onyx" }, false},
		{"nested assignee", func(env *fakeEnv) { env.wisps[0].Assignee = "gastown/polecats/onyx/x" }, false},
		{"parent still open", func(env *fakeEnv) {
			env.wisps[0].Parent = "gt-root"
			env.wispStatus["gt-root"] = "open"
		}, false},
		{"parent blocked", func(env *fakeEnv) {
			env.wisps[0].Parent = "gt-root"
			env.wispStatus["gt-root"] = "blocked"
		}, false},
		{"parent unreadable", func(env *fakeEnv) {
			env.wisps[0].Parent = "gt-root"
			env.wispStatusErr["gt-root"] = boom
		}, true},
		{"parent listed for a live seat", func(env *fakeEnv) {
			env.wisps[0].Parent = "gt-root"
			env.dirs["ruby"] = true
			env.wisps = append(env.wisps, leakedWisp("gt-root", "ruby", "open", 48*time.Hour))
		}, false},
		{"parent listed with another assignee", func(env *fakeEnv) {
			env.wisps[0].Parent = "gt-root"
			env.wisps = append(env.wisps, leakedWisp("gt-root", "opal", "open", 48*time.Hour))
		}, false},
		{"a younger step under it", func(env *fakeEnv) {
			step := leakedWisp("gt-step", "onyx", "open", time.Hour)
			step.Parent = "gt-wisp-a"
			env.wisps = append(env.wisps, step)
		}, false},
		{"parent cycle", func(env *fakeEnv) {
			env.wisps[0].Parent = "gt-wisp-b"
			b := leakedWisp("gt-wisp-b", "onyx", "open", 48*time.Hour)
			b.Parent = "gt-wisp-a"
			env.wisps = append(env.wisps, b)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			env.wisps = []Wisp{leakedWisp("gt-wisp-a", "onyx", "pinned", 48*time.Hour)}
			tc.setup(env)
			r := scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 {
				t.Fatalf("closed %v; want nothing closed", env.closed)
			}
			if got := r.Count(OutcomeUnknown) > 0; got != tc.wantUnknown {
				t.Fatalf("unknown findings = %v, want %v; report %v", got, tc.wantUnknown, r.Lines())
			}
		})
	}
}

// A step whose root was reaped, or closed, hangs under nothing live.
func TestLeakedWispWithAGoneOrClosedParentIsClosed(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]string{"reaped": "", "closed": "closed"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			w := leakedWisp("gt-step", "onyx", "pinned", 48*time.Hour)
			w.Parent = "gt-root"
			env.wisps = []Wisp{w}
			env.wispStatus["gt-root"] = status
			scanner(env, nil).Tick("gastown")
			if got := strings.Join(env.closed, ","); got != "gt-step" {
				t.Fatalf("closed = %q, want gt-step", got)
			}
		})
	}
}

// Closing a root closes its steps, so the pass makes one call for the tree.
func TestLeakedWispTreeIsClosedOnce(t *testing.T) {
	t.Parallel()
	env := newFake()
	root := leakedWisp("gt-root", "onyx", "open", 48*time.Hour)
	env.wisps = []Wisp{root}
	for _, id := range []string{"gt-step1", "gt-step2"} {
		step := leakedWisp(id, "onyx", "pinned", 48*time.Hour)
		step.Parent = "gt-root"
		env.wisps = append(env.wisps, step)
	}
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.closed, ","); got != "gt-root" {
		t.Fatalf("closed = %q, want one call on gt-root; report %v", got, r.Lines())
	}
	if !wispClosed(env, "gt-step1") || !wispClosed(env, "gt-step2") {
		t.Fatalf("steps left open: %+v", env.wisps)
	}
}

// leakedRoot is a molecule root no seat was assigned, created age ago.
func leakedRoot(id, status string, age time.Duration) Wisp {
	return Wisp{ID: id, Status: status, CreatedAt: now.Add(-age)}
}

// The steps of a molecule whose root was never assigned leak under that root:
// the root has no holder for the rule to read, so the steps decide and the
// root closes with them (gt-0tead).
func TestLeakedStepsUnderAnUnassignedRootCloseWithTheRoot(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.wisps = []Wisp{leakedRoot("gt-root", "open", 48*time.Hour)}
	for _, id := range []string{"gt-step1", "gt-step2"} {
		step := leakedWisp(id, "onyx", "pinned", 48*time.Hour)
		step.Parent = "gt-root"
		env.wisps = append(env.wisps, step)
	}
	// A step of a step too: the whole tree is read before the root closes.
	sub := leakedWisp("gt-sub", "onyx", "open", 48*time.Hour)
	sub.Parent = "gt-step2"
	env.wisps = append(env.wisps, sub)

	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.closed, ","); got != "gt-root" {
		t.Fatalf("closed = %q, want one call on gt-root; report %v", got, r.Lines())
	}
	for _, id := range []string{"gt-step1", "gt-step2", "gt-sub"} {
		if !wispClosed(env, id) {
			t.Errorf("%s is still not closed", id)
		}
	}
	if want := "Leaked wisp tree: molecule root has no holder (patrol scan)"; env.closeReasons[0] != want {
		t.Errorf("reason = %q, want %q", env.closeReasons[0], want)
	}
	if r.Count(OutcomeClosed) != 1 || r.Count(OutcomeUnknown) != 0 {
		t.Fatalf("report %v", r.Lines())
	}
}

func TestLeakedRootGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		setup       func(env *fakeEnv)
		wantUnknown bool
	}{
		{"a step held by a live seat", func(env *fakeEnv) {
			env.dirs["ruby"] = true
			env.wisps = append(env.wisps, stepUnderRoot("gt-other", "ruby", "in_progress", 48*time.Hour))
		}, false},
		{"a younger step", func(env *fakeEnv) {
			env.wisps = append(env.wisps, stepUnderRoot("gt-other", "onyx", "open", time.Hour))
		}, false},
		// Nothing holds the extra step either, so the tree is not the leak
		// shape the rule closes.
		{"a step with no assignee", func(env *fakeEnv) {
			env.wisps = append(env.wisps, Wisp{ID: "gt-other", Status: "open", Parent: "gt-root", CreatedAt: now.Add(-48 * time.Hour)})
		}, false},
		{"the root is younger than 24 hours", func(env *fakeEnv) {
			env.wisps[0].CreatedAt = now.Add(-time.Hour)
		}, false},
		{"the root is pinned", func(env *fakeEnv) { env.wisps[0].Status = "pinned" }, false},
		{"the root is in progress", func(env *fakeEnv) { env.wisps[0].Status = "in_progress" }, false},
		{"the root's age is unknown", func(env *fakeEnv) { env.wisps[0].CreatedAt = time.Time{} }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			env.wisps = []Wisp{leakedRoot("gt-root", "open", 48*time.Hour), stepUnderRoot("gt-step", "onyx", "pinned", 48*time.Hour)}
			tc.setup(env)
			r := scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 {
				t.Fatalf("closed %v; want nothing closed", env.closed)
			}
			if got := r.Count(OutcomeUnknown) > 0; got != tc.wantUnknown {
				t.Fatalf("unknown findings = %v, want %v; report %v", got, tc.wantUnknown, r.Lines())
			}
		})
	}
}

// A step whose root is left standing waits for the root: closing it alone
// would clear the wisp the next tick reads the tree from.
func TestLeakedStepUnderAKeptRootStaysOpen(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.wisps = []Wisp{
		leakedRoot("gt-root", "open", 48*time.Hour),
		stepUnderRoot("gt-step", "onyx", "pinned", 48*time.Hour),
	}
	env.dirs["ruby"] = true
	env.wisps = append(env.wisps, stepUnderRoot("gt-other", "ruby", "in_progress", 48*time.Hour))
	r := scanner(env, nil).Tick("gastown")
	if len(env.closed) != 0 || r.Count(OutcomeClosed) != 0 {
		t.Fatalf("closed %v; report %v", env.closed, r.Lines())
	}
}

func TestLeakedRootsCountTowardThePerTickCap(t *testing.T) {
	t.Parallel()
	env := newFake()
	for i := 0; i < 21; i++ {
		root := leakedRoot(fmt.Sprintf("gt-root-%02d", i), "open", 48*time.Hour)
		step := leakedWisp(fmt.Sprintf("gt-step-%02d", i), "onyx", "pinned", 48*time.Hour)
		step.Parent = root.ID
		env.wisps = append(env.wisps, root, step)
	}
	sc := scanner(env, nil)
	r := sc.Tick("gastown")
	if len(env.closed) != 20 {
		t.Fatalf("first tick closed %d, want 20", len(env.closed))
	}
	if r.Count(OutcomeSkipped) != 1 {
		t.Fatalf("report %v", r.Lines())
	}
	sc.Tick("gastown")
	if len(env.closed) != 21 {
		t.Fatalf("after the second tick closed %d, want 21", len(env.closed))
	}
}

func TestLeakedWispsCapClosesPerTick(t *testing.T) {
	t.Parallel()
	env := newFake()
	for i := 0; i < 25; i++ {
		env.wisps = append(env.wisps, leakedWisp(fmt.Sprintf("gt-wisp-%02d", i), "onyx", "pinned", 48*time.Hour))
	}
	sc := scanner(env, nil)
	sc.Tick("gastown")
	if len(env.closed) != 20 {
		t.Fatalf("first tick closed %d, want 20", len(env.closed))
	}
	sc.Tick("gastown")
	if len(env.closed) != 25 {
		t.Fatalf("after the second tick closed %d, want 25", len(env.closed))
	}
}

func TestLeakedWispCloseFailureIsReported(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.wisps = []Wisp{leakedWisp("gt-wisp-a", "onyx", "pinned", 48*time.Hour)}
	env.closeErr = errors.New("bd close: timeout")
	r := scanner(env, nil).Tick("gastown")
	if r.Count(OutcomeFailed) != 1 || r.Count(OutcomeClosed) != 0 || wispClosed(env, "gt-wisp-a") {
		t.Fatalf("report %v", r.Lines())
	}
}

// With no polecat holding a wisp the pass reads nothing past the listing.
func TestLeakedWispsQuietWhenNoPolecatHoldsOne(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.wisps = []Wisp{{ID: "gt-wisp-a", Status: "open", CreatedAt: now.Add(-48 * time.Hour)}}
	env.activeErr = errors.New("would fail if read")
	r := scanner(env, nil).Tick("gastown")
	if len(env.closed) != 0 || r.Count(OutcomeUnknown) != 0 {
		t.Fatalf("closed %v; report %v", env.closed, r.Lines())
	}
}

// moleculeRoot is an open, unassigned molecule root created age ago, whose
// blocks edge points at the work bead gt-work with the given status.
func moleculeRoot(env *fakeEnv, id, workStatus string, age time.Duration) {
	env.wisps = append(env.wisps, Wisp{ID: id, Status: "open", CreatedAt: now.Add(-age)})
	if workStatus != "" {
		env.rootWork = map[string][]WorkRef{id: {{ID: "gt-work", Status: workStatus}}}
	}
}

func TestClosedWorkRootIsClosedWithItsSteps(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"closed", "tombstone"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			moleculeRoot(env, "gt-wisp-root", status, 48*time.Hour)
			env.wisps = append(env.wisps,
				Wisp{ID: "gt-wisp-s1", Status: "open", Parent: "gt-wisp-root", CreatedAt: now.Add(-48 * time.Hour)},
				Wisp{ID: "gt-wisp-s2", Status: "in_progress", Parent: "gt-wisp-root", CreatedAt: now.Add(-1 * time.Hour)})
			// No polecat holds any wisp of the rig.
			env.activeErr = errors.New("would fail if read")
			r := scanner(env, nil).Tick("gastown")
			if got := strings.Join(env.closed, ","); got != "gt-wisp-root" {
				t.Fatalf("closed = %q; report %v", got, r.Lines())
			}
			for _, id := range []string{"gt-wisp-root", "gt-wisp-s1", "gt-wisp-s2"} {
				if !wispClosed(env, id) {
					t.Errorf("%s is still not closed", id)
				}
			}
			if r.Count(OutcomeClosed) != 1 || r.Count(OutcomeUnknown) != 0 {
				t.Fatalf("report %v", r.Lines())
			}
			if want := "Leaked wisp tree: work bead gt-work is " + status + " (patrol scan)"; env.closeReasons[0] != want {
				t.Errorf("reason = %q, want %q", env.closeReasons[0], want)
			}
		})
	}
}

// A root a live polecat holds is left alone even when its work bead is over:
// the polecat may be finishing its steps. The rule does not read its edge.
func TestClosedWorkRootLeavesRootALivePolecatHolds(t *testing.T) {
	t.Parallel()
	for _, live := range []string{"session", "directory"} {
		t.Run(live, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
			env.wisps[0].Assignee = "gastown/polecats/onyx"
			if live == "session" {
				env.sessions["onyx"] = true
			} else {
				env.dirs["onyx"] = true
			}
			r := scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 || len(env.rootReads) != 0 || r.Count(OutcomeUnknown) != 0 {
				t.Fatalf("closed %v, root reads %v; report %v", env.closed, env.rootReads, r.Lines())
			}
		})
	}
}

// A root whose polecat is gone goes once its work bead is over, even when the
// holder rules leave it (the seat still holds a hooked bead).
func TestClosedWorkRootClosesRootAGonePolecatHolds(t *testing.T) {
	t.Parallel()
	env := newFake()
	moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
	env.wisps[0].Assignee = "gastown/polecats/onyx"
	env.active = []Work{{ID: "gt-a", Status: "hooked", Assignee: "gastown/polecats/onyx"}}
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.closed, ","); got != "gt-wisp-root" {
		t.Fatalf("closed = %q; report %v", got, r.Lines())
	}
}

// A holder that cannot be read is Unknown, not live and not gone.
func TestClosedWorkRootUnreadableHolderIsUnknown(t *testing.T) {
	t.Parallel()
	env := newFake()
	moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
	env.wisps[0].Assignee = "gastown/polecats/onyx"
	env.active = []Work{{ID: "gt-a", Status: "hooked", Assignee: "gastown/polecats/onyx"}}
	env.sessionErr["onyx"] = errors.New("tmux: no server")
	r := scanner(env, nil).Tick("gastown")
	if len(env.closed) != 0 || len(env.rootReads) != 0 || r.Count(OutcomeUnknown) == 0 {
		t.Fatalf("closed %v, root reads %v; report %v", env.closed, env.rootReads, r.Lines())
	}
}

// A blocks edge to another wisp is not a work bead: a root that waits only on
// a closed wisp is not closed for it, and one that also waits on an open work
// bead is not closed for the wisp either.
func TestClosedWorkRootIgnoresEdgesToWisps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		work []WorkRef
	}{
		{"only a wisp", []WorkRef{{ID: "gt-wisp-other", Status: "closed"}}},
		{"wisp and open work bead", []WorkRef{{ID: "gt-wisp-other", Status: "closed"}, {ID: "gt-work", Status: "open"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
			env.rootWork["gt-wisp-root"] = tc.work
			r := scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 || r.Count(OutcomeUnknown) != 0 {
				t.Fatalf("closed %v; report %v", env.closed, r.Lines())
			}
		})
	}
	t.Run("wisp and closed work bead", func(t *testing.T) {
		t.Parallel()
		env := newFake()
		moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
		env.rootWork["gt-wisp-root"] = []WorkRef{{ID: "gt-wisp-other", Status: "open"}, {ID: "gt-work", Status: "closed"}}
		r := scanner(env, nil).Tick("gastown")
		if got := strings.Join(env.closed, ","); got != "gt-wisp-root" {
			t.Fatalf("closed = %q; report %v", got, r.Lines())
		}
	})
}

// The holder rules close a gone seat's tree from its unassigned root; the
// work-bead rule does not read or close that root a second time.
func TestClosedWorkRootSkipsRootTheHolderRulesClosed(t *testing.T) {
	t.Parallel()
	env := newFake()
	moleculeRoot(env, "gt-root", "closed", 48*time.Hour)
	env.wisps = append(env.wisps, stepUnderRoot("gt-wisp-a", "onyx", "open", 30*time.Hour))
	r := scanner(env, nil).Tick("gastown")
	if got := strings.Join(env.closed, ","); got != "gt-root" {
		t.Fatalf("closed = %q; report %v", got, r.Lines())
	}
	if len(env.rootReads) != 0 {
		t.Errorf("read edges of %v for a root the holder rules closed", env.rootReads)
	}
}

func TestClosedWorkRootGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(env *fakeEnv)
	}{
		{"work bead open", func(env *fakeEnv) { env.rootWork["gt-wisp-root"][0].Status = "open" }},
		{"work bead hooked", func(env *fakeEnv) { env.rootWork["gt-wisp-root"][0].Status = "hooked" }},
		{"work bead in_progress", func(env *fakeEnv) { env.rootWork["gt-wisp-root"][0].Status = "in_progress" }},
		{"one of two work beads still open", func(env *fakeEnv) {
			env.rootWork["gt-wisp-root"] = append(env.rootWork["gt-wisp-root"], WorkRef{ID: "gt-other", Status: "open"})
		}},
		{"pinned root with no work bead", func(env *fakeEnv) {
			env.wisps[0].Status = "pinned"
			delete(env.rootWork, "gt-wisp-root")
		}},
		{"open root with no work bead", func(env *fakeEnv) { delete(env.rootWork, "gt-wisp-root") }},
		{"pinned root with a closed work bead", func(env *fakeEnv) { env.wisps[0].Status = "pinned" }},
		{"young root", func(env *fakeEnv) { env.wisps[0].CreatedAt = now.Add(-23 * time.Hour) }},
		{"24 hours old exactly", func(env *fakeEnv) { env.wisps[0].CreatedAt = now.Add(-24 * time.Hour) }},
		{"a step, not a root", func(env *fakeEnv) { env.wisps[0].Parent = "gt-parent" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
			tc.setup(env)
			r := scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 || r.Count(OutcomeUnknown) != 0 || r.Count(OutcomeClosed) != 0 {
				t.Fatalf("closed %v; report %v", env.closed, r.Lines())
			}
		})
	}
}

func TestClosedWorkRootUnreadIsUnknown(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name  string
		setup func(env *fakeEnv)
	}{
		{"edge unreadable", func(env *fakeEnv) { env.rootWorkErr = map[string]error{"gt-wisp-root": boom} }},
		{"work bead status unreadable", func(env *fakeEnv) { env.rootWork["gt-wisp-root"][0].Status = "" }},
		{"one of two work beads unreadable", func(env *fakeEnv) {
			env.rootWork["gt-wisp-root"] = []WorkRef{{ID: "gt-a", Status: "closed"}, {ID: "gt-b"}}
		}},
		{"age unknown", func(env *fakeEnv) { env.wisps[0].CreatedAt = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
			tc.setup(env)
			r := scanner(env, nil).Tick("gastown")
			if len(env.closed) != 0 || r.Count(OutcomeUnknown) != 1 {
				t.Fatalf("closed %v; report %v", env.closed, r.Lines())
			}
		})
	}
}

func TestClosedWorkRootFailedCloseIsReported(t *testing.T) {
	t.Parallel()
	env := newFake()
	moleculeRoot(env, "gt-wisp-root", "closed", 48*time.Hour)
	env.closeErr = errors.New("bd: timeout")
	r := scanner(env, nil).Tick("gastown")
	if r.Count(OutcomeFailed) != 1 || r.Count(OutcomeClosed) != 0 {
		t.Fatalf("report %v", r.Lines())
	}
}

// The usual tick reads no edge: only an open root with no parent, older than
// a day, is a candidate.
func TestClosedWorkRootUsualTickReadsNothingExtra(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.wisps = []Wisp{
		{ID: "gt-wisp-young", Status: "open", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "gt-wisp-patrol", Status: "pinned", CreatedAt: now.Add(-90 * 24 * time.Hour)},
		{ID: "gt-wisp-step", Status: "open", Parent: "gt-wisp-young", CreatedAt: now.Add(-90 * 24 * time.Hour)},
	}
	r := scanner(env, nil).Tick("gastown")
	if len(env.rootReads) != 0 || len(env.closed) != 0 || r.Count(OutcomeUnknown) != 0 {
		t.Fatalf("root reads %v, closed %v; report %v", env.rootReads, env.closed, r.Lines())
	}
}

func TestClosedWorkRootsShareTheCloseCap(t *testing.T) {
	t.Parallel()
	env := newFake()
	env.rootWork = map[string][]WorkRef{}
	for i := 0; i < leakedWispMaxPerTick+3; i++ {
		id := fmt.Sprintf("gt-wisp-r%02d", i)
		env.wisps = append(env.wisps, Wisp{ID: id, Status: "open", CreatedAt: now.Add(-48 * time.Hour)})
		env.rootWork[id] = []WorkRef{{ID: "gt-work", Status: "closed"}}
	}
	r := scanner(env, nil).Tick("gastown")
	if len(env.closed) != leakedWispMaxPerTick || r.Count(OutcomeSkipped) != 1 {
		t.Fatalf("closed %d, want %d; report %v", len(env.closed), leakedWispMaxPerTick, r.Lines())
	}
}
