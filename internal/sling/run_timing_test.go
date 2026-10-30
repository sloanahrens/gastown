package sling

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// timingTownRoot and timingBeadID are the dispatch these tests run; the town is
// never read, so only the ids matter.
const (
	timingTownRoot = "/town"
	timingBeadID   = "gt-timing"
)

// timingClock is a clock the test moves by hand: every duration the trace
// records is one the fake collaborators below spent.
type timingClock struct{ now time.Time }

func newTimingClock() *timingClock {
	return &timingClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *timingClock) Now() time.Time        { return c.now }
func (c *timingClock) spend(d time.Duration) { c.now = c.now.Add(d) }

// timingDeps is a dispatch whose every stage spends time on clock, in the order
// the production path spends it: the engine's stages, the spawn's own steps,
// then the sandbox's phases and the launch.
type timingDeps struct {
	clock   *timingClock
	trace   *session.SandboxTrace
	spend   map[string]time.Duration
	hookErr error
}

// stage spends the named stage's time and nothing else.
func (f *timingDeps) stage(name string) { f.clock.spend(f.spend[name]) }

// step reports a stage through the spawner's step sink, as SlingSpawnOptions.step
// does on a timer.
func step(sink func(string), name string) {
	if sink != nil {
		sink(name)
	}
}

func (f *timingDeps) deps() *Deps {
	return &Deps{
		Out:      io.Discard,
		LockBead: func(string, string) (func(), error) { f.stage("guards"); return func() {}, nil },
		BeadInfoInTown: func(string, string) (*Bead, error) {
			f.stage("guards")
			return &Bead{Status: "open"}, nil
		},
		Actor:     func(Options) string { return "daemon/spec-dispatch" },
		Requester: func() string { return "daemon/spec-dispatch" },
		CheckDuplicates: func(string, string, *Bead) (*Duplicate, []DuplicateMatch, error) {
			f.stage("duplicate check")
			return nil, nil, nil
		},
		SpawnPolecat: func(_ string, opts SpawnOptions) (*Spawn, error) {
			// The production spawner marks the backpressure check and the seat
			// decision on the trace and reports the rest on its step timer; the
			// seat it claims sits between them.
			f.stage("spawn")
			f.trace.Mark("backpressure")
			f.stage("seat decision")
			f.trace.Mark("seat decision")
			step(opts.Steps, "admission")
			f.stage("worktree")
			step(opts.Steps, "worktree")
			return &Spawn{RigName: "gastown", PolecatName: "onyx"}, nil
		},
		AgentDead:          func(string) bool { return false },
		SurvivingWorkGuard: func(string, string, string) error { return nil },
		CollectMolecules:   func(*Bead, string, string) ([]string, error) { f.stage("molecule burn"); return nil, nil },
		RigCommandVars:     func(string, string) []string { return nil },
		PriorAttempt:       func(string, string) []string { return nil },
		Cook:               func(string, string, string) error { f.stage("formula cook"); return nil },
		InstantiateFormula: func(context.Context, string, string, string, string, string, []string) (*FormulaResult, error) {
			f.stage("formula")
			return &FormulaResult{BeadToHook: timingBeadID, WispRootID: "wisp-1"}, nil
		},
		LockAssignee:    func(string, string) (func(), error) { return func() {}, nil },
		ClearReassigned: func(string, string) func() { return func() {} },
		HookDir:         func(string, string, string) string { return "/town/hook" },
		Hook: func(string, string, string, string, *FieldUpdates) error {
			f.stage("hook")
			return f.hookErr
		},
		RecordReassignment: func(string, string, string, string, string) {},
		ClearOrphanLabels:  func(string, string, string) {},
		NoteDispatched:     func(string, *Duplicate) {},
		LogFeed:            func(string, string, map[string]interface{}) error { return nil },
		UpdateAgentHook:    func(string, string, string, string) {},
		UpdateAgentMode:    func(string, string, string, string) {},
		StoreFields:        func(string, string, FieldUpdates) error { return nil },
		RollbackArtifacts:  func(*Spawn, string, string, string) {},
		RestoreRawFields:   func(string, string, string, *Bead) {},
		ReleaseSeat:        func(*Spawn) {},
		StartSession: func(*Spawn) (string, error) {
			// The sandbox backend reports its own phases from inside the start,
			// the launch is marked when it returns, and the runtime wait after
			// it is the tail the engine's own "session start" stage names.
			_ = f.trace.Phase("push", func() error { f.stage("push"); return nil })
			_ = f.trace.Phase("create", func() error { f.stage("create"); return nil })
			f.stage("start")
			f.trace.Mark("start")
			f.stage("session start")
			return "%1", nil
		},
	}
}

func timingOptions(trace *session.SandboxTrace) Options {
	return Options{
		BeadID:      timingBeadID,
		TownRoot:    timingTownRoot,
		Agent:       "deepseek-flash",
		FormulaName: "mol-polecat-work",
		Steps:       trace.Steps(),
	}
}

// timingSpend is the time each stage of the fake dispatch takes, so a missing
// phase shows up as a sum short of the total rather than as an error.
func timingSpend() map[string]time.Duration {
	return map[string]time.Duration{
		"guards":          1 * time.Second,
		"duplicate check": 1 * time.Second,
		"molecule burn":   500 * time.Millisecond,
		"spawn":           4 * time.Second,
		"seat decision":   30 * time.Second,
		"worktree":        6 * time.Second,
		"formula cook":    2 * time.Second,
		"formula":         3 * time.Second,
		"hook":            2 * time.Second,
		"push":            2 * time.Second,
		"create":          4 * time.Second,
		"start":           1 * time.Second,
		"session start":   1 * time.Second,
	}
}

// TestTimerNamesEveryStageOfTheRun is the regression test for the untimed 40 to
// 50 seconds a spec sling's note left out (gt-7nctk): every stage the dispatch
// runs before the sandbox starts is named on the trace — including the seat
// decision, which can hold a sling up on its own — and the phases add up to the
// trace's total.
func TestTimerNamesEveryStageOfTheRun(t *testing.T) {
	t.Parallel()
	clock := newTimingClock()
	trace := session.NewSandboxTrace(clock.Now)
	f := &timingDeps{clock: clock, trace: trace, spend: timingSpend()}

	if _, err := Run(context.Background(), f.deps(), timingOptions(trace)); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	summary := trace.Summary()
	want := []string{
		"setup and guards", "duplicate check", "molecule burn", "backpressure", "seat decision",
		"admission", "worktree", "spawn", "formula cook", "formula", "hook",
		"push", "create", "start", "session start",
	}
	for _, name := range want {
		if !strings.Contains(summary, name+" ") {
			t.Errorf("Summary() = %q, want a phase named %q", summary, name)
		}
	}
	if share := timedShare(t, summary); share < 0.95 {
		t.Errorf("Summary() = %q records %.0f%% of the total, want at least 95%%: the rest of the sling is untimed", summary, share*100)
	}
	phases, total := parseTraceSummary(t, summary)
	if got, want := phases["seat decision"], 30*time.Second; got != want {
		t.Errorf("seat decision = %v, want %v: the seat decision is a stage of its own, not folded into the spawn", got, want)
	}
	if sum := phaseSum(phases); sum != total {
		t.Errorf("phases sum to %v of a %v total, want the whole dispatch: every interval is named", sum, total)
	}
}

// TestTimerNamesTheStageThatFails: a dispatch that stops at a step still shows
// that step's time, or the note would hide the most expensive failure.
func TestTimerNamesTheStageThatFails(t *testing.T) {
	t.Parallel()
	clock := newTimingClock()
	trace := session.NewSandboxTrace(clock.Now)
	f := &timingDeps{clock: clock, trace: trace, spend: timingSpend(), hookErr: errors.New("bd update refused")}

	if _, err := Run(context.Background(), f.deps(), timingOptions(trace)); err == nil {
		t.Fatal("a dispatch whose hook failed must fail")
	}

	summary := trace.Summary()
	phases, total := parseTraceSummary(t, summary)
	// The hook stage holds the write that failed and the hold re-read the
	// dispatch makes under the same stage, so its floor is the write's own time.
	if phases["hook"] < 2*time.Second {
		t.Errorf("Summary() = %q, want the failed stage's own time under hook", summary)
	}
	if sum := phaseSum(phases); sum != total {
		t.Errorf("phases sum to %v of a %v total, want the whole dispatch on the failure path too", sum, total)
	}
}

// TestTimerWithoutASinkNamesNoStage: the step sink is the caller's, and a sling
// that wants no timing — every hand-run gt sling — passes none. The engine must
// name no stage then, or the stages would reach output the sling never had.
func TestTimerWithoutASinkNamesNoStage(t *testing.T) {
	t.Parallel()
	clock := newTimingClock()
	trace := session.NewSandboxTrace(clock.Now)
	f := &timingDeps{clock: clock, trace: trace, spend: timingSpend()}
	opts := timingOptions(trace)
	opts.Steps = nil

	if _, err := Run(context.Background(), f.deps(), opts); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	// The spawn and the session start still record their own sandbox phases;
	// what must not happen is the engine naming stages nobody asked to time.
	if got := trace.Summary(); strings.Contains(got, "setup and guards") || strings.Contains(got, "spawn") {
		t.Errorf("Summary() = %q, want no engine stage: the caller passed no sink", got)
	}
}

// parseTraceSummary reads a trace summary into its phases and its total.
func parseTraceSummary(t *testing.T, summary string) (map[string]time.Duration, time.Duration) {
	t.Helper()
	parts := strings.Split(summary, ", ")
	if len(parts) == 0 || !strings.HasPrefix(parts[len(parts)-1], "total ") {
		t.Fatalf("Summary() = %q, want a trailing total", summary)
	}
	total := parseTraceDuration(t, strings.TrimPrefix(parts[len(parts)-1], "total "))
	phases := make(map[string]time.Duration, len(parts)-1)
	for _, p := range parts[:len(parts)-1] {
		// A phase name may hold spaces of its own, so the duration is the part
		// after the last one.
		cut := strings.LastIndex(p, " ")
		if cut <= 0 {
			t.Fatalf("Summary() = %q, want every phase to name a duration", summary)
		}
		phases[p[:cut]] = parseTraceDuration(t, p[cut+1:])
	}
	return phases, total
}

func phaseSum(phases map[string]time.Duration) time.Duration {
	var sum time.Duration
	for _, took := range phases {
		sum += took
	}
	return sum
}

// timedShare reports the share of a trace summary's total that its phases
// account for.
func timedShare(t *testing.T, summary string) float64 {
	t.Helper()
	phases, total := parseTraceSummary(t, summary)
	if total <= 0 {
		t.Fatalf("Summary() = %q, want a positive total", summary)
	}
	return float64(phaseSum(phases)) / float64(total)
}

func parseTraceDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		if nanos, nerr := strconv.ParseInt(strings.TrimSuffix(s, "ns"), 10, 64); nerr == nil && strings.HasSuffix(s, "ns") {
			return time.Duration(nanos)
		}
		t.Fatalf("cannot parse %q as a duration: %v", s, err)
	}
	return d
}
