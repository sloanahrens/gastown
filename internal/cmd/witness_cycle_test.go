package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/patrolstate"
)

var wcNow = time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)

// fakeWitnessCycle is an in-memory witness: no tmux, beads, mail or town.
type fakeWitnessCycle struct {
	env         map[string]string
	paneSession string
	sessionID   string
	wait        *patrolstate.WaitOutcome
	waitErr     error
	state       patrolstate.CycleState
	loadErr     error
	saveErr     error
	reportErr   error
	respawnErr  error
	handoffAge  time.Duration
	hasHandoff  bool

	seq         []string // order of report / save / record / respawn
	drained     []bool   // drainNudges passed to each report
	saved       []patrolstate.CycleState
	escalations []string
}

func (f *fakeWitnessCycle) deps(out *bytes.Buffer) witnessCycleDeps {
	return witnessCycleDeps{
		Report: func(drain bool) error {
			f.seq = append(f.seq, "report")
			f.drained = append(f.drained, drain)
			return f.reportErr
		},
		SessionID: func() string { return f.sessionID },
		ReadWait:  func() (*patrolstate.WaitOutcome, error) { return f.wait, f.waitErr },
		LoadState: func() (patrolstate.CycleState, error) { return f.state, f.loadErr },
		SaveState: func(s patrolstate.CycleState) error {
			f.seq = append(f.seq, "save")
			if f.saveErr != nil {
				return f.saveErr
			}
			f.saved = append(f.saved, s)
			f.state = s
			return nil
		},
		RecordCycle: func() { f.seq = append(f.seq, "record") },
		Respawn: func() error {
			f.seq = append(f.seq, "respawn")
			return f.respawnErr
		},
		Escalate:    func(fp, sev, msg string) { f.escalations = append(f.escalations, fp+"|"+sev) },
		PaneSession: func() (string, error) { return f.paneSession, nil },
		HandoffAge:  func() (time.Duration, bool) { return f.handoffAge, f.hasHandoff },
		Getenv:      func(k string) string { return f.env[k] },
		Now:         func() time.Time { return wcNow },
		Out:         out,
	}
}

func witnessFake() *fakeWitnessCycle {
	return &fakeWitnessCycle{
		env:         map[string]string{"GT_ROLE": "gastown/witness", "TMUX_PANE": "%3"},
		paneSession: "gt-witness",
		sessionID:   "sess-1",
	}
}

func witnessParams() witnessCycleParams {
	return witnessCycleParams{
		Rig: "gastown", Session: "gt-witness", WorkDir: "/town/gastown/witness",
		Enabled: true, MinCycles: 3, MaxCycles: 8,
	}
}

// waitAt builds this cycle's await-signal outcome, written after the last
// consumed one.
func waitAt(reason string, atCap bool) *patrolstate.WaitOutcome {
	w := &patrolstate.WaitOutcome{Reason: reason, BackoffMax: 5 * time.Minute, SessionID: "sess-1", At: wcNow}
	switch {
	case reason == "timeout" && atCap:
		w.Timeout, w.AtCap = 5*time.Minute, true
	case reason == "timeout":
		w.Timeout = 2 * time.Minute
	}
	return w
}

// priorCycles makes the fake session look as if it already completed n
// reports, so this report completes cycle n+1.
func (f *fakeWitnessCycle) priorCycles(n int) *fakeWitnessCycle {
	f.state = patrolstate.CycleState{SessionID: f.sessionID, Cycles: n, LastWaitAt: wcNow.Add(-10 * time.Minute)}
	return f
}

func TestWitnessCycle_BoundaryTruthTable(t *testing.T) {
	cases := []struct {
		name    string
		prior   int // completed before this report; this report is cycle prior+1
		wait    *patrolstate.WaitOutcome
		respawn bool
	}{
		{"cap timeout, cycle 1", 0, waitAt("timeout", true), false},
		{"cap timeout, cycle 2", 1, waitAt("timeout", true), false},
		{"cap timeout, cycle 3", 2, waitAt("timeout", true), true},
		{"cap timeout, cycle 6", 5, waitAt("timeout", true), true},
		{"non-cap timeout, cycle 5", 4, waitAt("timeout", false), false},
		{"event wake, cycle 5", 4, waitAt("signal", false), false},
		{"no wait recorded, cycle 5", 4, nil, false},
		{"event wake, cycle 7", 6, waitAt("signal", false), false},
		{"backstop: event wake, cycle 8", 7, waitAt("signal", false), true},
		{"backstop: non-cap timeout, cycle 8", 7, waitAt("timeout", false), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := witnessFake().priorCycles(tc.prior)
			f.wait = tc.wait
			var out bytes.Buffer
			rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&out))
			if err != nil {
				t.Fatal(err)
			}
			if rep.Respawned != tc.respawn {
				t.Fatalf("respawned=%v, want %v (cause %q)\n%s", rep.Respawned, tc.respawn, rep.SkipCause, out.String())
			}
			if tc.respawn {
				if got := strings.Join(f.seq, ","); got != "report,save,record,respawn" {
					t.Fatalf("order = %s, want report,save,record,respawn", got)
				}
				if f.drained[0] {
					t.Fatal("report drained nudges right before a respawn; they would be lost")
				}
				if f.state.Cycles != 0 {
					t.Fatalf("counter persisted as %d before respawn, want 0", f.state.Cycles)
				}
				return
			}
			if got := strings.Join(f.seq, ","); got != "report,save" {
				t.Fatalf("order = %s, want report,save", got)
			}
			if !f.drained[0] {
				t.Fatal("a kept session's report must drain nudges as today")
			}
			if f.state.Cycles != tc.prior+1 {
				t.Fatalf("counter = %d, want %d", f.state.Cycles, tc.prior+1)
			}
			if !strings.Contains(out.String(), "○ session kept: ") || rep.SkipCause == "" {
				t.Fatalf("a skip must print its cause:\n%s", out.String())
			}
		})
	}
}

func TestWitnessCycle_ThreeQuietCyclesThenRespawnThenCountRestarts(t *testing.T) {
	f := witnessFake()
	p := witnessParams()
	for i := 1; i <= 3; i++ {
		w := waitAt("timeout", true)
		w.At = wcNow.Add(time.Duration(i) * time.Minute)
		f.wait = w
		f.seq = nil
		rep, err := reportAndMaybeCycleWitness(p, f.deps(&bytes.Buffer{}))
		if err != nil {
			t.Fatal(err)
		}
		if rep.Respawned != (i == 3) {
			t.Fatalf("cycle %d respawned=%v", i, rep.Respawned)
		}
	}
	// The successor: a new runtime session. The predecessor's wait is not
	// reused, and the count starts again at 1.
	f.sessionID = "sess-2"
	f.seq = nil
	rep, err := reportAndMaybeCycleWitness(p, f.deps(&bytes.Buffer{}))
	if err != nil || rep.Respawned {
		t.Fatalf("successor's first report respawned (err %v)", err)
	}
	if f.state.SessionID != "sess-2" || f.state.Cycles != 1 {
		t.Fatalf("successor state = %+v, want sess-2 cycle 1", f.state)
	}
}

func TestWitnessCycle_CounterResetsOnFreshSession(t *testing.T) {
	// A predecessor far past the minimum (it died without respawning here):
	// the fresh session's first cap timeout must not respawn it.
	f := witnessFake()
	f.state = patrolstate.CycleState{SessionID: "old", Cycles: 7}
	f.wait = waitAt("timeout", true)
	rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{}))
	if err != nil || rep.Respawned {
		t.Fatalf("fresh session respawned on its first cycle (err %v)", err)
	}
	if f.state.SessionID != "sess-1" || f.state.Cycles != 1 {
		t.Fatalf("state = %+v, want the count restarted for sess-1", f.state)
	}
}

func TestWitnessCycle_UnknownSessionIDStillResetsAtRespawn(t *testing.T) {
	// No SessionStart hook recorded an ID: the respawn itself zeroes the count.
	f := witnessFake().priorCycles(2)
	f.sessionID = ""
	f.state.SessionID = ""
	f.wait = waitAt("timeout", true)
	f.wait.SessionID = ""
	rep, _ := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{}))
	if !rep.Respawned || f.state.Cycles != 0 {
		t.Fatalf("respawned=%v state=%+v, want a respawn with the count zeroed", rep.Respawned, f.state)
	}
}

func TestWitnessCycle_SameWaitIsNotCountedTwice(t *testing.T) {
	// The agent skipped await-signal: the report must not reuse the last wait.
	f := witnessFake().priorCycles(4)
	f.wait = waitAt("timeout", true)
	f.state.LastWaitAt = f.wait.At // already consumed
	rep, _ := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{}))
	if rep.Respawned || !strings.Contains(rep.SkipCause, "no await-signal outcome") {
		t.Fatalf("report = %+v, want kept with no wait attributed", rep)
	}
}

func TestWitnessCycle_FlagOffNeverRespawnsOrCounts(t *testing.T) {
	for _, prior := range []int{2, 7, 50} {
		f := witnessFake().priorCycles(prior)
		f.wait = waitAt("timeout", true)
		p := witnessParams()
		p.Enabled = false
		var out bytes.Buffer
		rep, err := reportAndMaybeCycleWitness(p, f.deps(&out))
		if err != nil || rep.Respawned {
			t.Fatalf("flag off respawned (prior %d, err %v)", prior, err)
		}
		if got := strings.Join(f.seq, ","); got != "report" || !f.drained[0] {
			t.Fatalf("flag off did %s (drain %v), want exactly today's report", got, f.drained)
		}
		if out.Len() != 0 {
			t.Fatalf("flag off printed %q; it must behave exactly as today", out.String())
		}
	}
}

func TestWitnessCycle_GuardsKeepTheSession(t *testing.T) {
	cases := map[string]func(f *fakeWitnessCycle){
		"human caller":      func(f *fakeWitnessCycle) { f.env["GT_ROLE"] = "gastown/crew/sloan" },
		"other rig witness": func(f *fakeWitnessCycle) { f.env["GT_ROLE"] = "hm/witness" },
		"refinery":          func(f *fakeWitnessCycle) { f.env["GT_ROLE"] = "gastown/refinery" },
		"not in tmux":       func(f *fakeWitnessCycle) { delete(f.env, "TMUX_PANE") },
		"wrong pane":        func(f *fakeWitnessCycle) { f.paneSession = "gt-crew-sloan" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := witnessFake().priorCycles(7)
			f.wait = waitAt("timeout", true)
			mutate(f)
			var out bytes.Buffer
			rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&out))
			if err != nil || rep.Respawned {
				t.Fatalf("guard %s respawned (err %v)", name, err)
			}
			if got := strings.Join(f.seq, ","); got != "report" {
				t.Fatalf("guard %s did %s; a foreign caller must not touch the witness counter", name, got)
			}
			if !strings.Contains(out.String(), "○ session kept: ") {
				t.Fatalf("guard skip was silent:\n%s", out.String())
			}
		})
	}
}

func TestWitnessCycle_CooldownSkipsWithoutSleeping(t *testing.T) {
	f := witnessFake().priorCycles(2)
	f.wait = waitAt("timeout", true)
	f.hasHandoff, f.handoffAge = true, 30*time.Second
	start := time.Now()
	var out bytes.Buffer
	rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&out))
	if err != nil || rep.Respawned {
		t.Fatalf("respawned inside the cooldown (err %v)", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cooldown slept; it must skip")
	}
	if !strings.Contains(rep.SkipCause, "last handoff 30s ago") || !strings.Contains(out.String(), "○ session kept: last handoff") {
		t.Fatalf("cause = %q\n%s", rep.SkipCause, out.String())
	}
	if !f.drained[0] || f.state.Cycles != 3 {
		t.Fatalf("drain=%v cycles=%d; a cooldown skip is a kept session and keeps counting", f.drained, f.state.Cycles)
	}

	// An old handoff is not a cooldown.
	f = witnessFake().priorCycles(2)
	f.wait = waitAt("timeout", true)
	f.hasHandoff, f.handoffAge = true, 10*time.Minute
	if rep, _ := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{})); !rep.Respawned {
		t.Fatalf("old handoff blocked the respawn: %+v", rep)
	}
}

func TestWitnessCycle_ReportFailureNeitherCountsNorRespawns(t *testing.T) {
	f := witnessFake().priorCycles(7)
	f.wait = waitAt("timeout", true)
	f.reportErr = errors.New("dolt down")
	_, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{}))
	if err == nil {
		t.Fatal("report error was swallowed")
	}
	if got := strings.Join(f.seq, ","); got != "report" {
		t.Fatalf("after a failed report did %s", got)
	}
}

func TestWitnessCycle_SaveFailureKeepsTheSession(t *testing.T) {
	f := witnessFake().priorCycles(2)
	f.wait = waitAt("timeout", true)
	f.saveErr = errors.New("disk full")
	rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{}))
	if err != nil || rep.Respawned {
		t.Fatalf("respawned without persisting the reset counter (err %v)", err)
	}
	if strings.Contains(strings.Join(f.seq, ","), "record") {
		t.Fatalf("recorded a cycle that never happened: %v", f.seq)
	}
}

func TestWitnessCycle_RespawnFailureEscalates(t *testing.T) {
	f := witnessFake().priorCycles(2)
	f.wait = waitAt("timeout", true)
	f.respawnErr = errors.New("no pane")
	rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&bytes.Buffer{}))
	if err != nil || rep.Respawned || !strings.HasPrefix(rep.SkipCause, "respawn failed") {
		t.Fatalf("report = %+v err %v", rep, err)
	}
	if len(f.escalations) != 1 || f.escalations[0] != "witness-respawn-failed:gastown|medium" {
		t.Fatalf("escalations = %v", f.escalations)
	}
}

func TestWitnessCycle_UnreadableFilesNeverCauseARespawn(t *testing.T) {
	f := witnessFake()
	f.loadErr = errors.New("corrupt")
	f.waitErr = errors.New("corrupt")
	var out bytes.Buffer
	rep, err := reportAndMaybeCycleWitness(witnessParams(), f.deps(&out))
	if err != nil || rep.Respawned || f.state.Cycles != 1 {
		t.Fatalf("rep=%+v state=%+v err=%v", rep, f.state, err)
	}
	if !strings.Contains(out.String(), "cycle counter unreadable") || !strings.Contains(out.String(), "await-signal outcome unreadable") {
		t.Fatalf("unreadable files were not warned:\n%s", out.String())
	}
}

func TestPatrolCycleDir(t *testing.T) {
	town := t.TempDir()
	rigDir := filepath.Join(town, "gastown")
	if err := os.MkdirAll(filepath.Join(rigDir, "witness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := patrolCycleDir(town, "gastown/witness"); got != "" {
		t.Fatalf("flag off (no config) = %q, want \"\"", got)
	}
	cfg := `{"type":"rig","version":1,"name":"gastown","witness":{"cycle_session_at_idle_cap":true}}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := patrolCycleDir(town, "gastown/witness"), filepath.Join(rigDir, "witness"); got != want {
		t.Fatalf("witness dir = %q, want %q", got, want)
	}
	for _, role := range []string{"", "gastown/refinery", "deacon", "gastown/polecats/nux", "nosuchrig/witness", "a/b/witness"} {
		if got := patrolCycleDir(town, role); got != "" {
			t.Errorf("patrolCycleDir(%q) = %q, want \"\"", role, got)
		}
	}
	if _, err := os.Stat(filepath.Join(town, "nosuchrig")); !os.IsNotExist(err) {
		t.Fatal("created a stray rig directory")
	}
}

func TestWitnessRespawnEffortLine(t *testing.T) {
	abbrev := &patrolstate.WaitOutcome{Reason: "timeout", IdleCycles: 6, EffortLevel: "abbreviated"}
	full := &patrolstate.WaitOutcome{Reason: "signal", EffortLevel: "full"}
	if line := witnessRespawnEffortLine("unit-cycle", abbrev); !strings.HasPrefix(line, "EFFORT: reduced") || !strings.Contains(line, "ABBREVIATED") {
		t.Fatalf("line = %q", line)
	}
	for name, tc := range map[string]struct {
		reason string
		wait   *patrolstate.WaitOutcome
	}{
		"full effort":     {"unit-cycle", full},
		"no wait":         {"unit-cycle", nil},
		"not a respawn":   {"", abbrev},
		"compaction path": {"compaction", abbrev},
	} {
		if line := witnessRespawnEffortLine(tc.reason, tc.wait); line != "" {
			t.Errorf("%s: line = %q, want none", name, line)
		}
	}
	if len(witnessRespawnEffortLine("unit-cycle", abbrev)) > 120 {
		t.Error("EFFORT line must stay small (prime hook budget)")
	}
}

func TestOwnPaneCallerMismatchAndCooldownCause(t *testing.T) {
	env := map[string]string{"GT_ROLE": "gastown/witness", "TMUX_PANE": "%1"}
	getenv := func(k string) string { return env[k] }
	pane := func() (string, error) { return "gt-witness", nil }
	if c := ownPaneCallerMismatch("gastown/witness", "gt-witness", getenv, pane); c != "" {
		t.Fatalf("own pane = %q, want \"\"", c)
	}
	if c := ownPaneCallerMismatch("gastown/witness", "gt-witness", getenv,
		func() (string, error) { return "", errors.New("no server") }); !strings.Contains(c, "pane session unknown") {
		t.Fatalf("pane error = %q", c)
	}
	if c := handoffCooldownCause(func() (time.Duration, bool) { return 0, false }, "x"); c != "" {
		t.Fatalf("no handoff = %q", c)
	}
	if c := handoffCooldownCause(func() (time.Duration, bool) { return 90 * time.Second, true }, "later"); !strings.HasSuffix(c, "; later") {
		t.Fatalf("cooldown = %q", c)
	}
}

func TestRecordAwaitSignalOutcome(t *testing.T) {
	town := t.TempDir()
	rigDir := filepath.Join(town, "gastown")
	witnessDir := filepath.Join(rigDir, "witness")
	if err := os.MkdirAll(filepath.Join(witnessDir, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig","version":1,"name":"gastown","witness":{"cycle_session_at_idle_cap":true}}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(witnessDir, ".runtime", "session_id"), []byte("sess-9\n2026-09-25T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_ROLE", "gastown/witness")

	cases := []struct {
		name          string
		result        AwaitSignalResult
		full, backoff time.Duration
		timedOutAtCap bool
	}{
		{"cap timeout", AwaitSignalResult{Reason: "timeout", IdleCycles: 6, EffortLevel: "abbreviated"}, 5 * time.Minute, 5 * time.Minute, true},
		{"below cap", AwaitSignalResult{Reason: "timeout", IdleCycles: 2}, 2 * time.Minute, 5 * time.Minute, false},
		{"signal at cap", AwaitSignalResult{Reason: "signal"}, 5 * time.Minute, 5 * time.Minute, false},
		{"no cap configured", AwaitSignalResult{Reason: "timeout"}, time.Minute, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.result
			recordAwaitSignalOutcome(town, &r, tc.full, tc.backoff)
			got, err := patrolstate.ReadWaitOutcome(witnessDir)
			if err != nil || got == nil {
				t.Fatalf("read = (%v, %v)", got, err)
			}
			if got.TimedOutAtCap() != tc.timedOutAtCap || got.SessionID != "sess-9" || got.Reason != r.Reason {
				t.Fatalf("outcome = %+v, want timed-out-at-cap %v for sess-9", got, tc.timedOutAtCap)
			}
		})
	}

	// Another role writes nothing.
	if err := os.Remove(patrolstate.WaitOutcomePath(witnessDir)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_ROLE", "gastown/refinery")
	recordAwaitSignalOutcome(town, &AwaitSignalResult{Reason: "timeout"}, 5*time.Minute, 5*time.Minute)
	if _, err := os.Stat(patrolstate.WaitOutcomePath(witnessDir)); !os.IsNotExist(err) {
		t.Fatal("a non-witness caller wrote the witness wait outcome")
	}
}
