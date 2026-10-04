package patrolscan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
)

// fakeReapEnv adds the ReapEnv half to the shared fakeEnv.
type fakeReapEnv struct {
	*fakeEnv
	recovery    map[string]Recovery
	recoveryErr map[string]error
	idleSince   map[string]time.Time
	idleErr     map[string]error
	claimed     map[string]string // branch -> bead
	claimErr    map[string]error
	reapErr     map[string]error
	reaped      []string
}

func (f *fakeReapEnv) Recovery(_, p string) (Recovery, error) { return f.recovery[p], f.recoveryErr[p] }
func (f *fakeReapEnv) IdleSince(_, p string) (time.Time, error) {
	return f.idleSince[p], f.idleErr[p]
}
func (f *fakeReapEnv) BranchClaimed(_, b string) (string, error) { return f.claimed[b], f.claimErr[b] }
func (f *fakeReapEnv) Reap(_, p string) error {
	if err := f.reapErr[p]; err != nil {
		return err
	}
	f.reaped = append(f.reaped, p)
	return nil
}

// eligible returns an env with one seat, "onyx", that passes every veto and is
// safe to remove: idle an hour, parked-free, SAFE_TO_NUKE measured live.
func eligible() *fakeReapEnv {
	base := newFake()
	base.polecats = []string{"onyx"}
	return &fakeReapEnv{
		fakeEnv:     base,
		recovery:    map[string]Recovery{"onyx": {Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "live", Branch: "polecat/onyx/gt-1"}},
		recoveryErr: map[string]error{},
		idleSince:   map[string]time.Time{"onyx": now.Add(-time.Hour)},
		idleErr:     map[string]error{},
		claimed:     map[string]string{},
		claimErr:    map[string]error{},
		reapErr:     map[string]error{},
	}
}

func reapScanner(env *fakeReapEnv, o ReapOptions) *Scanner {
	return New(env, memLedger{}, Options{
		Now:       func() time.Time { return now },
		IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		Reap:      &o,
	})
}

func reapFinding(t *testing.T, r Report, p string) (Finding, bool) {
	t.Helper()
	for _, f := range r.Findings {
		if f.Kind == "reap" && f.Subject == p {
			return f, true
		}
	}
	return Finding{}, false
}

func TestReapEligibleSeatIsRemoved(t *testing.T) {
	t.Parallel()
	env := eligible()
	r := reapScanner(env, ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeReaped {
		t.Fatalf("finding = %+v, %v; want reaped", f, ok)
	}
	if len(env.reaped) != 1 || env.reaped[0] != "onyx" {
		t.Fatalf("reaped = %v, want [onyx]", env.reaped)
	}
}

func TestReapDryRunNeverRemoves(t *testing.T) {
	t.Parallel()
	env := eligible()
	r := reapScanner(env, ReapOptions{DryRun: true}).Tick("gastown")
	if f, ok := reapFinding(t, r, "onyx"); !ok || f.Outcome != OutcomeWouldReap {
		t.Fatalf("finding = %+v, %v; want would-reap", f, ok)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("dry-run removed %v", env.reaped)
	}
}

// Each row breaks exactly one fact of the eligible baseline. quiet means no
// reap finding at all; otherwise the outcome must match. Reap is never called.
func TestReapVetoes(t *testing.T) {
	t.Parallel()
	boom := errors.New("read failed")
	rows := []struct {
		name   string
		mutate func(*fakeReapEnv)
		quiet  bool
		want   Outcome
	}{
		{"intent unreadable", func(e *fakeReapEnv) { e.intentErr = map[string]error{"onyx": boom} }, false, OutcomeUnknown},
		{"frozen", func(e *fakeReapEnv) { e.intents = map[string]intent.Record{"onyx": {Frozen: true}} }, true, ""},
		{"submitted", func(e *fakeReapEnv) { e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredSubmitted}} }, true, ""},
		{"live session", func(e *fakeReapEnv) { e.sessions = map[string]bool{"onyx": true} }, true, ""},
		{"session read fails", func(e *fakeReapEnv) { e.sessionErr = map[string]error{"onyx": boom} }, false, OutcomeUnknown},
		{"fresh heartbeat", func(e *fakeReapEnv) {
			e.heartbeats = map[string]*Heartbeat{"onyx": {State: "working", At: now.Add(-time.Minute)}}
		}, true, ""},
		{"assigned bead", func(e *fakeReapEnv) { e.work = map[string]*Work{"onyx": {ID: "gt-9", Status: "in_progress"}} }, true, ""},
		{"assigned bead unreadable", func(e *fakeReapEnv) { e.workErr = map[string]error{"onyx": boom} }, false, OutcomeUnknown},
		{"recovery unreadable", func(e *fakeReapEnv) { e.recoveryErr["onyx"] = boom }, false, OutcomeUnknown},
		{"branch claimed by open bead", func(e *fakeReapEnv) { e.claimed["polecat/onyx/gt-1"] = "gt-2" }, false, OutcomeSkipped},
		{"branch claim unreadable", func(e *fakeReapEnv) { e.claimErr["polecat/onyx/gt-1"] = boom }, false, OutcomeUnknown},
		{"open MR", func(e *fakeReapEnv) { e.recovery["onyx"] = Recovery{Verdict: "PENDING_MR", GitStateSource: "live"} }, true, ""},
		{"working verdict", func(e *fakeReapEnv) { e.recovery["onyx"] = Recovery{Verdict: "WORKING", GitStateSource: "live"} }, true, ""},
		{"reusable seat is capacity", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
		}, true, ""},
		{"idle-since unknown", func(e *fakeReapEnv) { e.idleSince["onyx"] = time.Time{} }, false, OutcomeSkipped},
		{"idle-since unreadable", func(e *fakeReapEnv) { e.idleErr["onyx"] = boom }, false, OutcomeUnknown},
		{"inside grace", func(e *fakeReapEnv) { e.idleSince["onyx"] = now.Add(-29 * time.Minute) }, true, ""},
		{"parked inside parked grace", func(e *fakeReapEnv) {
			e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			e.idleSince["onyx"] = now.Add(-23 * time.Hour)
		}, true, ""},
		{"parked reusable inside parked grace", func(e *fakeReapEnv) {
			e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			e.idleSince["onyx"] = now.Add(-23 * time.Hour)
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
		}, true, ""},
		{"parked reusable with recorded git state", func(e *fakeReapEnv) {
			e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			e.idleSince["onyx"] = now.Add(-25 * time.Hour)
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "recorded"}
		}, false, OutcomeBlocked},
		{"parked reusable needing recovery", func(e *fakeReapEnv) {
			e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			e.idleSince["onyx"] = now.Add(-25 * time.Hour)
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reusable: true, Blockers: []string{"3 unpushed commits"}, GitStateSource: "live"}
		}, false, OutcomeBlocked},
		{"frozen parked seat", func(e *fakeReapEnv) {
			e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark, Frozen: true}}
			e.idleSince["onyx"] = now.Add(-25 * time.Hour)
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
		}, true, ""},
		{"git state only recorded", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "recorded"}
		}, false, OutcomeBlocked},
		{"git state unknown", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "unknown"}
		}, false, OutcomeBlocked},
		{"needs recovery", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Blockers: []string{"3 unpushed commits"}, GitStateSource: "live"}
		}, false, OutcomeBlocked},
		{"needs mq submit", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_MQ_SUBMIT", GitStateSource: "live"}
		}, false, OutcomeBlocked},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			row.mutate(env)
			r := reapScanner(env, ReapOptions{}).Tick("gastown")
			f, ok := reapFinding(t, r, "onyx")
			if row.quiet && ok {
				t.Fatalf("want no reap finding, got %+v", f)
			}
			if !row.quiet && (!ok || f.Outcome != row.want) {
				t.Fatalf("finding = %+v, %v; want %s", f, ok, row.want)
			}
			if len(env.reaped) != 0 {
				t.Fatalf("Reap called for %v", env.reaped)
			}
		})
	}
}

func TestReapGraceBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		parked bool
		idle   time.Duration
		reaped bool
	}{
		{"ordinary 31m", false, 31 * time.Minute, true},
		{"parked 25h", true, 25 * time.Hour, true},
		{"parked 31m is not enough", true, 31 * time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			env.idleSince["onyx"] = now.Add(-c.idle)
			if c.parked {
				env.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			}
			reapScanner(env, ReapOptions{}).Tick("gastown")
			if got := len(env.reaped) == 1; got != c.reaped {
				t.Fatalf("reaped = %v, want %v", env.reaped, c.reaped)
			}
		})
	}
}

// A parked seat whose verdict says Reusable is not reusable capacity: the
// pause marker is read by the allocator and `gt polecat list`, not by
// check-recovery, so the seat lands here reported reusable yet must still
// reach would-reap (dry run) and reaped (live) once idle past ParkedGrace.
func TestReapParkedReusableSeatIsReapable(t *testing.T) {
	t.Parallel()
	for _, dry := range []bool{true, false} {
		env := eligible()
		env.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
		env.idleSince["onyx"] = now.Add(-25 * time.Hour)
		env.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live", Branch: "polecat/onyx/gt-1"}
		r := reapScanner(env, ReapOptions{DryRun: dry}).Tick("gastown")
		want := OutcomeReaped
		if dry {
			want = OutcomeWouldReap
		}
		f, ok := reapFinding(t, r, "onyx")
		if !ok || f.Outcome != want {
			t.Fatalf("dry=%v: finding = %+v, %v; want %s", dry, f, ok, want)
		}
		if reaped := len(env.reaped) == 1; reaped == dry {
			t.Fatalf("dry=%v: reaped = %v", dry, env.reaped)
		}
	}
}

func TestReapPerTickCap(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.polecats = []string{"a", "b", "c", "d", "e"}
	for _, p := range env.polecats {
		env.recovery[p] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "live"}
		env.idleSince[p] = now.Add(-time.Hour)
	}
	reapScanner(env, ReapOptions{}).Tick("gastown") // MaxPerTick unset => 2
	if len(env.reaped) != 2 {
		t.Fatalf("reaped %v, want exactly 2 (default cap)", env.reaped)
	}
	env2 := eligible()
	env2.polecats = env.polecats
	env2.recovery, env2.idleSince = env.recovery, env.idleSince
	reapScanner(env2, ReapOptions{MaxPerTick: 4}).Tick("gastown")
	if len(env2.reaped) != 4 {
		t.Fatalf("reaped %v, want 4", env2.reaped)
	}
}

func TestReapNukeRefusalIsBlockedNotFailed(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.reapErr["onyx"] = errRefused
	r := reapScanner(env, ReapOptions{}).Tick("gastown")
	if f, _ := reapFinding(t, r, "onyx"); f.Outcome != OutcomeBlocked {
		t.Fatalf("outcome = %s, want blocked", f.Outcome)
	}
	env = eligible()
	env.reapErr["onyx"] = errors.New("exec failed")
	r = reapScanner(env, ReapOptions{}).Tick("gastown")
	if f, _ := reapFinding(t, r, "onyx"); f.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", f.Outcome)
	}
}

// The Q1 line, pinned: today's town has 12 done, clean, reuse-eligible seats.
// None of them may be reaped or reported as would-reap.
func TestReapNeverTouchesReusableDoneSeats(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.polecats = nil
	for _, p := range []string{"agate", "basalt", "malachite", "marble", "mica", "pearl", "pyrite", "sapphire", "shale", "slate", "topaz", "turquoise"} {
		env.polecats = append(env.polecats, p)
		env.recovery[p] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
		env.idleSince[p] = now.Add(-72 * time.Hour)
	}
	for _, dry := range []bool{true, false} {
		r := reapScanner(env, ReapOptions{DryRun: dry}).Tick("gastown")
		if n := r.Count(OutcomeReaped) + r.Count(OutcomeWouldReap) + r.Count(OutcomeBlocked); n != 0 {
			t.Fatalf("dry=%v: %d reap findings on reusable seats: %+v", dry, n, r.Findings)
		}
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped reusable seats: %v", env.reaped)
	}
}

func TestReapOffWhenOptionOrEnvMissing(t *testing.T) {
	t.Parallel()
	env := eligible()
	New(env, memLedger{}, Options{Now: func() time.Time { return now }}).Tick("gastown") // Reap nil
	if len(env.reaped) != 0 {
		t.Fatalf("reaped with Options.Reap nil: %v", env.reaped)
	}
	// A plain fakeEnv has no ReapEnv half: the tick must still run.
	plain := newFake()
	plain.polecats = []string{"onyx"}
	o := ReapOptions{}
	New(plain, memLedger{}, Options{Now: func() time.Time { return now }, Reap: &o}).Tick("gastown")
}

func TestSummaryLineCountsReapOutcomes(t *testing.T) {
	t.Parallel()
	r := Report{Rig: "gastown", Findings: []Finding{
		{Kind: "reap", Outcome: OutcomeReaped},
		{Kind: "reap", Outcome: OutcomeWouldReap},
		{Kind: "reap", Outcome: OutcomeBlocked},
		{Kind: "reap", Outcome: OutcomeBlocked},
	}}
	want := "1 reaped, 1 would-reap, 2 blocked"
	if got := r.Lines()[0]; !strings.Contains(got, want) {
		t.Fatalf("summary %q lacks %q", got, want)
	}
}
