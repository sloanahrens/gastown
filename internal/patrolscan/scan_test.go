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
	states      map[string]string
	stateErr    map[string]error
	heartbeats  map[string]*Heartbeat
	restartErr  map[string]error
	idleErr     map[string]error
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

	restarts []string
	idles    []string // polecats whose seat was retired to stop
	closed   []string
	comments map[string][]string
	reads    []string // AgentState reads, to prove they are refusal-only
}

func newFake() *fakeEnv {
	return &fakeEnv{
		intents: map[string]intent.Record{}, intentErr: map[string]error{},
		verdicts: map[string]liveness.Result{}, work: map[string]*Work{}, workErr: map[string]error{},
		states: map[string]string{}, stateErr: map[string]error{}, heartbeats: map[string]*Heartbeat{},
		restartErr: map[string]error{}, idleErr: map[string]error{}, idleStopped: map[string]bool{},
		dirs: map[string]bool{}, dirErr: map[string]error{},
		sessions: map[string]bool{}, sessionErr: map[string]error{}, molStatus: map[string]string{},
		molErr: map[string]error{}, branches: map[string]string{}, branchErr: map[string]error{},
		comments: map[string][]string{},
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
func (f *fakeEnv) AssignedWork(_, p string) (*Work, error) { return f.work[p], f.workErr[p] }
func (f *fakeEnv) AgentState(_, p string) (string, error) {
	f.reads = append(f.reads, p)
	return f.states[p], f.stateErr[p]
}
func (f *fakeEnv) Heartbeat(_, p string) *Heartbeat { return f.heartbeats[p] }
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
	env.states[p] = "working"
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

func TestNoRestartCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(env *fakeEnv)
		outcome Outcome // "" = no finding
	}{
		{"submitted intent (hazard 1, landing guard)", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ruby"}
		}, ""},
		{"parked seat", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredPark, Reason: "operator"}
		}, ""},
		{"frozen seat (budget spent earlier)", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Frozen: true}
		}, ""},
		{"unreadable intent record", func(env *fakeEnv) {
			env.intentErr["ruby"] = errors.New("permission denied")
		}, OutcomeUnknown},
		{"agent_state stuck (hazard 1)", func(env *fakeEnv) { env.states["ruby"] = "stuck" }, OutcomeSkipped},
		{"agent_state awaiting-gate (hazard 1)", func(env *fakeEnv) { env.states["ruby"] = "awaiting-gate" }, OutcomeSkipped},
		{"agent_state paused (hazard 1)", func(env *fakeEnv) { env.states["ruby"] = "paused" }, OutcomeSkipped},
		{"agent_state done", func(env *fakeEnv) { env.states["ruby"] = "done" }, OutcomeSkipped},
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

// Held and submitted seats are decided from the intent record alone: no
// liveness sample, no bd read.
func TestHeldSeatReadsNothingElse(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.intents["ruby"] = intent.Record{Desired: intent.DesiredSubmitted}
	scanner(env, nil).Tick("gastown")
	if len(env.reads) != 0 {
		t.Fatalf("agent bead read %d time(s) for a submitted seat", len(env.reads))
	}
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

func TestStrandedBranchIsReportedOncePerWindow(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branches["gt-a"] = "polecat/onyx/gt-a@abc"
	ledger := memLedger{}
	s := scanner(env, ledger)

	s.Tick("gastown")
	s.Tick("gastown")
	if n := len(env.comments["gt-a"]); n != 1 {
		t.Fatalf("comments = %d, want exactly 1 within the window", n)
	}
	c := env.comments["gt-a"][0]
	for _, want := range []string{"polecat/onyx/gt-a@abc", "do not re-sling with --force"} {
		if !strings.Contains(c, want) {
			t.Fatalf("comment %q missing %q", c, want)
		}
	}

	// A window later it may report again.
	ledger["gastown/gt-a"] = now.Add(-DefaultReportWindow - time.Minute)
	s.Tick("gastown")
	if n := len(env.comments["gt-a"]); n != 2 {
		t.Fatalf("comments = %d after the window, want 2", n)
	}
}

func TestStrandedNeverRedispatches(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	scanner(env, nil).Tick("gastown")
	c := strings.Join(env.comments["gt-a"], "\n")
	if !strings.Contains(c, "No polecat branch carries unlanded work") || strings.Contains(c, "gt sling") {
		t.Fatalf("comment = %q", c)
	}
	if len(env.restarts) != 0 {
		t.Fatalf("restarted %v", env.restarts)
	}
}

func TestSurvivingWorkUnknownWritesNothing(t *testing.T) {
	t.Parallel()
	env := newFake()
	goneHolder(env, "gt-a", "onyx")
	env.branchErr["gt-a"] = errors.New("ls-remote timed out")
	ledger := memLedger{}
	r := scanner(env, ledger).Tick("gastown")
	if len(env.comments) != 0 || len(ledger) != 0 {
		t.Fatalf("wrote a report on an unknown survival answer: %v", env.comments)
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
