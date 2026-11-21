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
	preserved   map[string]string // seat -> the ref its nuke pushed the tip to
	reaped      []string
}

func (f *fakeReapEnv) Recovery(_, p string) (Recovery, error) { return f.recovery[p], f.recoveryErr[p] }
func (f *fakeReapEnv) IdleSince(_, p string) (time.Time, error) {
	return f.idleSince[p], f.idleErr[p]
}
func (f *fakeReapEnv) BranchClaimed(_, b string) (string, error) { return f.claimed[b], f.claimErr[b] }
func (f *fakeReapEnv) Reap(_, p string) (string, error) {
	if err := f.reapErr[p]; err != nil {
		return "", err
	}
	f.reaped = append(f.reaped, p)
	return f.preserved[p], nil
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

func reapScanner(env Env, o ReapOptions) *Scanner {
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

// A reaped seat's detail names the ref its nuke preserved the tip on, and adds
// nothing when the nuke pushed nothing (gt-edepiy).
func TestReapDetailNamesPreservedRef(t *testing.T) {
	t.Parallel()
	const ref = "origin/polecat/onyx/gt-1-abc1234"
	const base = "idle 1h0m0s, SAFE_TO_NUKE, git state live"
	for _, row := range []struct{ preserved, detail string }{
		{ref, base + ", preserved as " + ref},
		{"", base},
	} {
		env := eligible()
		env.preserved = map[string]string{"onyx": row.preserved}
		r := reapScanner(env, ReapOptions{}).Tick("gastown")
		f, ok := reapFinding(t, r, "onyx")
		if !ok || f.Outcome != OutcomeReaped {
			t.Fatalf("preserved %q: finding = %+v, %v; want reaped", row.preserved, f, ok)
		}
		if f.Detail != row.detail {
			t.Fatalf("preserved %q: detail = %q, want %q", row.preserved, f.Detail, row.detail)
		}
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
			// The measured shape; this host names no sandbox seat (gt-aimwqy).
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "git-unpushed", Blockers: []string{"git_state=has_unpushed unpushed_commits=3"}, GitStateSource: "live"}
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

// TestReapVetoesASubmittedVerdict: a seat whose work waits for the landing
// worker is quiet, keeps its branch and is never reaped — under the rig's
// directory cap and at it, where the trim lifts only the reusable veto. The
// record says run because gt done's record write is best-effort (gt-xs1ni).
func TestReapVetoesASubmittedVerdict(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		env  func(*fakeReapEnv) Env
	}{
		{"under the directory cap", func(e *fakeReapEnv) Env { return e }},
		{"named by the trim at the cap", func(e *fakeReapEnv) Env { return trimmable(e, "onyx") }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			env.intents["onyx"] = intent.Record{WorkBead: "gt-1"}
			env.recovery["onyx"] = Recovery{Verdict: "SUBMITTED", Reason: "submitted-for-landing", Branch: "polecat/onyx/gt-1"}
			r := reapScanner(row.env(env), ReapOptions{}).Tick("gastown")
			if f, ok := reapFinding(t, r, "onyx"); ok {
				t.Fatalf("want no reap finding for a submitted verdict, got %+v", f)
			}
			if len(env.reaped) != 0 {
				t.Fatalf("Reap called for a submitted verdict: %v", env.reaped)
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

// fakeTrimEnv adds the directory-cap trim half to fakeReapEnv.
type fakeTrimEnv struct {
	*fakeReapEnv
	trimmable []string
	trimErr   error
	// listed records the polecats the tick passed to TrimmableSeats.
	listed []string
}

func (f *fakeTrimEnv) TrimmableSeats(_ string, polecats []string) ([]string, error) {
	f.listed = polecats
	return f.trimmable, f.trimErr
}

// trimmable wraps env with the trim half a host at its directory cap answers
// with: names are the seats the rig's cap may trim this tick.
func trimmable(e *fakeReapEnv, names ...string) *fakeTrimEnv {
	return &fakeTrimEnv{fakeReapEnv: e, trimmable: names}
}

// reusableSeat makes onyx a live, clean, reusable seat: the capacity the pass
// never reaps unless the directory cap trims it.
func reusableSeat(e *fakeReapEnv) {
	e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live", Branch: "polecat/onyx/gt-1"}
}

// fakeSingleUseEnv adds the single-use sandbox half to fakeReapEnv and records
// the seats it was asked about.
type fakeSingleUseEnv struct {
	*fakeReapEnv
	sandbox map[string]bool
	err     map[string]error
	reads   []string
}

func (f *fakeSingleUseEnv) SandboxSeat(_, p string) (bool, error) {
	f.reads = append(f.reads, p)
	return f.sandbox[p], f.err[p]
}

func singleUseSeat(e *fakeReapEnv, sandbox bool) *fakeSingleUseEnv {
	return &fakeSingleUseEnv{fakeReapEnv: e, sandbox: map[string]bool{"onyx": sandbox}, err: map[string]error{}}
}

// TestReapReapsAFinishedSandboxSeatOutsideTheCap guards gt-2fi5ek: a sandbox
// seat is single-use, so a done, reusable, safe seat past its grace is not
// capacity and is removed under the rig's directory cap.
func TestReapReapsAFinishedSandboxSeatOutsideTheCap(t *testing.T) {
	t.Parallel()
	for _, dry := range []bool{true, false} {
		env := eligible()
		reusableSeat(env)
		r := reapScanner(singleUseSeat(env, true), ReapOptions{DryRun: dry}).Tick("gastown")
		want := OutcomeReaped
		if dry {
			want = OutcomeWouldReap
		}
		f, ok := reapFinding(t, r, "onyx")
		if !ok || f.Outcome != want {
			t.Fatalf("dry=%v: finding = %+v, %v; want %s", dry, f, ok, want)
		}
		if !strings.Contains(f.Detail, "single-use sandbox seat") {
			t.Errorf("dry=%v: detail = %q; want the single-use seat named", dry, f.Detail)
		}
		if reaped := len(env.reaped) == 1; reaped == dry {
			t.Fatalf("dry=%v: reaped = %v", dry, env.reaped)
		}
	}
}

// TestReapLeavesAReusableNonSandboxSeatAlone: a reusable seat the read says is
// not a sandbox seat is capacity as before, and the read waits for the grace so
// a pool seat inside it costs no gateway read (gt-2fi5ek).
func TestReapLeavesAReusableNonSandboxSeatAlone(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	tick := singleUseSeat(env, false)
	r := reapScanner(tick, ReapOptions{}).Tick("gastown")
	if f, ok := reapFinding(t, r, "onyx"); ok {
		t.Fatalf("finding = %+v; want a quiet skip", f)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v; want none", env.reaped)
	}
	if len(tick.reads) != 1 {
		t.Fatalf("SandboxSeat read %v past the grace; want exactly one read", tick.reads)
	}
	inside := eligible()
	reusableSeat(inside)
	inside.idleSince["onyx"] = now.Add(-29 * time.Minute)
	tick2 := singleUseSeat(inside, true)
	reapScanner(tick2, ReapOptions{}).Tick("gastown")
	if len(tick2.reads) != 0 {
		t.Fatalf("SandboxSeat read %v inside the grace; want no read", tick2.reads)
	}
}

// TestReapSandboxSeatVetoesStillHold guards gt-2fi5ek: the reusable veto it
// lifts is the only one a single-use sandbox seat is free of. Each row breaks
// one fact the pass measures about the seat's work, and Reap is never called.
func TestReapSandboxSeatVetoesStillHold(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		mutate func(*fakeReapEnv)
		quiet  bool
		want   Outcome
	}{
		{"live session", func(e *fakeReapEnv) { e.sessions = map[string]bool{"onyx": true} }, true, ""},
		{"fresh heartbeat", func(e *fakeReapEnv) {
			e.heartbeats = map[string]*Heartbeat{"onyx": {State: "working", At: now.Add(-time.Minute)}}
		}, true, ""},
		{"open assigned work", func(e *fakeReapEnv) {
			e.work = map[string]*Work{"onyx": {ID: "gt-9", Status: "in_progress"}}
		}, true, ""},
		{"branch claimed by open bead", func(e *fakeReapEnv) { e.claimed["polecat/onyx/gt-1"] = "gt-2" }, false, OutcomeSkipped},
		{"verdict not safe to nuke", func(e *fakeReapEnv) {
			// An at-risk second blocker; unpushed commits alone reap now.
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", SafeToNuke: true, Reusable: true, Blockers: []string{"git_state=has_unpushed unpushed_commits=3", "git_state=has_stash stash_count=1"}, GitStateSource: "live", Branch: "polecat/onyx/gt-1"}
		}, false, OutcomeBlocked},
		{"git state not measured live", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "recorded", Branch: "polecat/onyx/gt-1"}
		}, false, OutcomeBlocked},
		{"idle under the grace", func(e *fakeReapEnv) { e.idleSince["onyx"] = now.Add(-29 * time.Minute) }, true, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			reusableSeat(env)
			row.mutate(env)
			r := reapScanner(singleUseSeat(env, true), ReapOptions{}).Tick("gastown")
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

// TestReapSandboxSeatReadFailureIsUnknown: a sandbox-seat read that fails is
// Unknown, never read as "not a sandbox seat", and nothing is removed
// (gt-2fi5ek).
func TestReapSandboxSeatReadFailureIsUnknown(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	fake := singleUseSeat(env, true)
	fake.err["onyx"] = errors.New("gateway unreadable")
	r := reapScanner(fake, ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeUnknown {
		t.Fatalf("finding = %+v, %v; want unknown", f, ok)
	}
	if !strings.Contains(f.Detail, "sandbox seat unreadable") {
		t.Errorf("detail = %q; want the failed read named", f.Detail)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v on an unreadable sandbox seat", env.reaped)
	}
}

// TestReapWithoutSingleUseEnvIsUnchanged: a host with no SingleUseEnv has no
// sandbox seats to reap, so a reusable done seat is capacity as before
// (gt-2fi5ek).
func TestReapWithoutSingleUseEnvIsUnchanged(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	r := reapScanner(env, ReapOptions{}).Tick("gastown")
	if f, ok := reapFinding(t, r, "onyx"); ok {
		t.Fatalf("finding = %+v; want a quiet skip", f)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v; want none", env.reaped)
	}
}

// unpushedSeat makes onyx a NEEDS_RECOVERY seat on the git-unpushed reason.
func unpushedSeat(e *fakeReapEnv, blockers ...string) {
	e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "git-unpushed", GitStateSource: "live",
		Blockers: blockers, Branch: "polecat/onyx/gt-1"}
}

// TestReapReapsASandboxSeatWithAMeasuredUnpushedBlocker guards gt-aimwqy: the
// nuke pushes the tip to origin before it removes the seat, so a sandbox seat
// whose only blocker is its unpushed commits is handed to Reap, and the nuke's
// own check and preserving push decide.
//
// The blocker is production's one entry: workstate.go appends the landed
// probe's detail to it, so the text after "; " is part of the same blocker.
func TestReapReapsASandboxSeatWithAMeasuredUnpushedBlocker(t *testing.T) {
	t.Parallel()
	env := eligible()
	unpushedSeat(env, "git_state=has_unpushed unpushed_commits=1; bead gt-1 is not closed")
	r := reapScanner(singleUseSeat(env, true), ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeReaped {
		t.Fatalf("finding = %+v, %v; want reaped", f, ok)
	}
	if !strings.Contains(f.Detail, "single-use sandbox seat") {
		t.Errorf("detail = %q; want the single-use seat named", f.Detail)
	}
	if len(env.reaped) != 1 || env.reaped[0] != "onyx" {
		t.Fatalf("reaped = %v, want [onyx]", env.reaped)
	}
}

// TestReapReapsASandboxSeatWithARecordedAndAMeasuredUnpushedBlocker guards
// gt-aimwqy: the recorded cleanup status is a blocker entry of its own beside
// the measured one, and together they still name only work the nuke preserves.
func TestReapReapsASandboxSeatWithARecordedAndAMeasuredUnpushedBlocker(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "cleanup-has_unpushed", GitStateSource: "live",
		Blockers: []string{"cleanup_status=has_unpushed", "git_state=has_unpushed unpushed_commits=1"}, Branch: "polecat/onyx/gt-1"}
	r := reapScanner(singleUseSeat(env, true), ReapOptions{}).Tick("gastown")
	if f, ok := reapFinding(t, r, "onyx"); !ok || f.Outcome != OutcomeReaped {
		t.Fatalf("finding = %+v, %v; want reaped", f, ok)
	}
	if len(env.reaped) != 1 || env.reaped[0] != "onyx" {
		t.Fatalf("reaped = %v, want [onyx]", env.reaped)
	}
}

// TestReapBlocksAnUnpushedSeatWhoseSandboxSeatReadFailed guards gt-ssd8zb: a
// seat the host cannot classify is not handed to the nuke, and its verdict
// still reads Blocked — never Unknown, which would lose why it needs recovery.
func TestReapBlocksAnUnpushedSeatWhoseSandboxSeatReadFailed(t *testing.T) {
	t.Parallel()
	env := eligible()
	unpushedSeat(env, "git_state=has_unpushed unpushed_commits=1")
	tick := singleUseSeat(env, true)
	tick.err["onyx"] = errors.New("gateway unreadable")
	r := reapScanner(tick, ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeBlocked {
		t.Fatalf("finding = %+v, %v; want blocked", f, ok)
	}
	for _, want := range []string{"verdict NEEDS_RECOVERY", "sandbox seat unreadable: gateway unreadable"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q; want %q", f.Detail, want)
		}
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v on an unreadable sandbox seat", env.reaped)
	}
}

// TestReapBlocksAnUnpushedSeatWhoseGitCheckFailed guards gt-ssd8zb: a git check
// that failed measured no unpushed count, so the seat has no route to the nuke
// and stays Blocked. The blocker is production's shape for a failed sandbox
// probe (sandbox_seat_probe.go), and the git state source is "unknown".
func TestReapBlocksAnUnpushedSeatWhoseGitCheckFailed(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "git-check-failed", GitStateSource: "unknown",
		Blockers: []string{"git_state=unknown: sandbox probe: gateway read failed"}, Branch: "polecat/onyx/gt-1"}
	r := reapScanner(singleUseSeat(env, true), ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeBlocked {
		t.Fatalf("finding = %+v, %v; want blocked", f, ok)
	}
	if !strings.Contains(f.Detail, "git_state=unknown: sandbox probe: gateway read failed") {
		t.Errorf("detail = %q; want the failed git check named", f.Detail)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v on a seat whose git check failed", env.reaped)
	}
}

// TestReapBlocksASandboxUnpushedSeatWithAnyOtherBlocker guards gt-aimwqy: only
// the measured unpushed commits are the nuke's to preserve, so a second at-risk
// blocker entry (production writes one per cause) keeps the seat blocked.
func TestReapBlocksASandboxUnpushedSeatWithAnyOtherBlocker(t *testing.T) {
	t.Parallel()
	const unpushed = "git_state=has_unpushed unpushed_commits=1"
	rows := []struct {
		name    string
		mutate  func(*fakeReapEnv)
		sandbox bool
		want    Outcome
	}{
		{"uncommitted file", func(e *fakeReapEnv) { unpushedSeat(e, unpushed, "git_state=has_uncommitted") }, true, OutcomeBlocked},
		{"stash", func(e *fakeReapEnv) { unpushedSeat(e, unpushed, "git_state=has_stash stash_count=1") }, true, OutcomeBlocked},
		{"push failed", func(e *fakeReapEnv) { unpushedSeat(e, unpushed, "push_failed=true") }, true, OutcomeBlocked},
		{"hook still set", func(e *fakeReapEnv) { unpushedSeat(e, unpushed, "has work on hook (gt-1)") }, true, OutcomeBlocked},
		{"open merge request", func(e *fakeReapEnv) { unpushedSeat(e, unpushed, "active-mr-open pr#12") }, true, OutcomeBlocked},
		{"not a sandbox seat", func(e *fakeReapEnv) { unpushedSeat(e, unpushed) }, false, OutcomeBlocked},
		{"unpushed text mid-blocker", func(e *fakeReapEnv) { unpushedSeat(e, "x "+unpushed) }, true, OutcomeBlocked},
		{"recorded status alone", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "cleanup-has_unpushed", GitStateSource: "live", Blockers: []string{"cleanup_status=has_unpushed"}}
		}, true, OutcomeBlocked},
		{"verdict not needs-recovery", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_MQ_SUBMIT", Reason: "git-unpushed", GitStateSource: "live", Blockers: []string{unpushed}}
		}, true, OutcomeBlocked},
		{"reason not git-unpushed", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "landed-differs", GitStateSource: "live", Blockers: []string{unpushed}}
		}, true, OutcomeBlocked},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			row.mutate(env)
			r := reapScanner(singleUseSeat(env, row.sandbox), ReapOptions{}).Tick("gastown")
			f, ok := reapFinding(t, r, "onyx")
			if !ok || f.Outcome != row.want {
				t.Fatalf("finding = %+v, %v; want %s", f, ok, row.want)
			}
			if len(env.reaped) != 0 {
				t.Fatalf("reaped %v", env.reaped)
			}
		})
	}
}

// TestReapBlocksASandboxUnpushedSeatOnANukeRefusalAndFailsOnOtherNukeErrors
// guards gt-aimwqy: an exit-3 nuke refusal is Blocked; any other failure is Failed.
func TestReapBlocksASandboxUnpushedSeatOnANukeRefusalAndFailsOnOtherNukeErrors(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		err  error
		want Outcome
	}{{"nuke refused", errRefused, OutcomeBlocked}, {"nuke failed", errors.New("exec failed"), OutcomeFailed}}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			unpushedSeat(env, "git_state=has_unpushed unpushed_commits=1")
			env.reapErr["onyx"] = row.err
			if f, ok := reapFinding(t, reapScanner(singleUseSeat(env, true), ReapOptions{}).Tick("gastown"), "onyx"); !ok || f.Outcome != row.want {
				t.Fatalf("finding = %+v, %v; want %s", f, ok, row.want)
			}
			if len(env.reaped) != 0 {
				t.Fatalf("reaped %v", env.reaped)
			}
		})
	}
}

// TestReapBlocksASandboxUnpushedSeatWithARecordedGitState guards gt-aimwqy: the
// nuke's route needs a live git state, so a recorded one keeps the seat blocked.
func TestReapBlocksASandboxUnpushedSeatWithARecordedGitState(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "git-unpushed", GitStateSource: "recorded",
		Blockers: []string{"git_state=has_unpushed unpushed_commits=1"}, Branch: "polecat/onyx/gt-1"}
	if f, ok := reapFinding(t, reapScanner(singleUseSeat(env, true), ReapOptions{}).Tick("gastown"), "onyx"); !ok || f.Outcome != OutcomeBlocked {
		t.Fatalf("finding = %+v, %v; want blocked", f, ok)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v", env.reaped)
	}
}

// TestDirCapTrimReapsIdleCleanSandboxSeat guards gt-xlazq. The seat is reusable
// capacity, which the pass leaves alone, and the host names it trimmable
// because the rig is at its directory cap: the removal is what admits a new
// seat.
func TestDirCapTrimReapsIdleCleanSandboxSeat(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	r := reapScanner(trimmable(env, "onyx"), ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeReaped {
		t.Fatalf("finding = %+v, %v; want reaped", f, ok)
	}
	if !strings.Contains(f.Detail, "trimmed at the rig's directory cap") {
		t.Errorf("detail = %q; want it to name the trim", f.Detail)
	}
	if len(env.reaped) != 1 || env.reaped[0] != "onyx" {
		t.Fatalf("reaped = %v, want [onyx]", env.reaped)
	}
}

// TestDirCapTrimOffUnderTheCap pins Q1: a rig under its cap may not trim a
// reusable seat, and the host says so by naming none.
func TestDirCapTrimOffUnderTheCap(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	r := reapScanner(trimmable(env), ReapOptions{}).Tick("gastown")
	if f, ok := reapFinding(t, r, "onyx"); ok {
		t.Fatalf("finding = %+v; want a quiet skip under the cap", f)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v under the cap", env.reaped)
	}
}

// TestDirCapTrimReapsTheOldestIdleSeatFirst: the per-tick removal budget goes
// to the seat that has been idle longest, not the first name in the listing.
func TestDirCapTrimReapsTheOldestIdleSeatFirst(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.polecats = []string{"a", "b", "c"}
	env.recovery = map[string]Recovery{}
	env.idleSince = map[string]time.Time{"a": now.Add(-time.Hour), "b": now.Add(-3 * time.Hour), "c": now.Add(-2 * time.Hour)}
	for _, p := range env.polecats {
		env.recovery[p] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
	}
	reapScanner(trimmable(env, "a", "b", "c"), ReapOptions{MaxPerTick: 1}).Tick("gastown")
	if len(env.reaped) != 1 || env.reaped[0] != "b" {
		t.Fatalf("reaped = %v, want [b] (idle three hours, the oldest)", env.reaped)
	}
}

// TestDirCapTrimNeverReapsPreservedWork: the trim lifts the reusable veto and
// nothing else. A seat not proven safe, or whose branch another bead claims, is
// not removed — except a sandbox seat whose only blocker is unpushed commits,
// which the nuke preserves on origin: gt-xlazq's rule, reversed for sandbox
// seats on the overseer's decision of 2026-11-19 (gt-aimwqy).
func TestDirCapTrimNeverReapsPreservedWork(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		mutate func(*fakeReapEnv)
		quiet  bool
		want   Outcome
	}{
		{"unpushed work", func(e *fakeReapEnv) {
			// The trim names the seat, but the host names no sandbox seat.
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reason: "git-unpushed", Reusable: true, Blockers: []string{"git_state=has_unpushed unpushed_commits=3"}, GitStateSource: "live"}
		}, false, OutcomeBlocked},
		{"uncommitted files", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Reusable: true, Blockers: []string{"2 uncommitted files"}, GitStateSource: "live"}
		}, false, OutcomeBlocked},
		{"git state only recorded", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "recorded"}
		}, false, OutcomeBlocked},
		{"branch claimed by open bead", func(e *fakeReapEnv) {
			reusableSeat(e)
			e.claimed["polecat/onyx/gt-1"] = "gt-2"
		}, false, OutcomeSkipped},
		{"live session", func(e *fakeReapEnv) {
			reusableSeat(e)
			e.sessions = map[string]bool{"onyx": true}
		}, true, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			env := eligible()
			reusableSeat(env)
			row.mutate(env)
			r := reapScanner(trimmable(env, "onyx"), ReapOptions{}).Tick("gastown")
			f, ok := reapFinding(t, r, "onyx")
			if row.quiet && ok {
				t.Fatalf("want no reap finding, got %+v", f)
			}
			if !row.quiet && (!ok || f.Outcome != row.want) {
				t.Fatalf("finding = %+v, %v; want %s", f, ok, row.want)
			}
			if len(env.reaped) != 0 {
				t.Fatalf("reaped %v with work at risk", env.reaped)
			}
		})
	}
}

// TestDirCapTrimFailsClosed: a rig whose cap or seat kinds cannot be read trims
// nothing, and says so on the report rather than reading the failed read as a
// rig under its cap.
func TestDirCapTrimFailsClosed(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	r := reapScanner(&fakeTrimEnv{fakeReapEnv: env, trimErr: errors.New("cap unreadable")}, ReapOptions{}).Tick("gastown")
	if len(r.Errors) == 0 || !strings.Contains(strings.Join(r.Errors, " "), "cap unreadable") {
		t.Fatalf("errors = %v; want the unreadable cap reported", r.Errors)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped %v with the cap unreadable", env.reaped)
	}
}

// TestDirCapTrimReadsTheTicksListing guards gt-spsmb: the trim classifies the
// polecats the tick already listed instead of asking the host to list them a
// second time.
func TestDirCapTrimReadsTheTicksListing(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.polecats = []string{"agate", "onyx"}
	env.recovery["agate"] = Recovery{Verdict: "WORKING"}
	trim := trimmable(env, "onyx")
	reapScanner(trim, ReapOptions{}).Tick("gastown")
	if got, want := strings.Join(trim.listed, ","), strings.Join(env.polecats, ","); got != want {
		t.Fatalf("TrimmableSeats got %q, want the tick's listing %q", got, want)
	}
}

// TestDirCapTrimOffWhenTheListingFails: the tick's listing is the trim's only
// input, so a listing that cannot be read trims nothing and says so — the
// failed read is never the empty rig the no-trim answer looks like.
func TestDirCapTrimOffWhenTheListingFails(t *testing.T) {
	t.Parallel()
	env := eligible()
	env.polecatsErr = errors.New("polecats unreadable")
	trim := trimmable(env, "onyx")
	r := reapScanner(trim, ReapOptions{}).Tick("gastown")
	if len(r.Errors) == 0 || !strings.Contains(strings.Join(r.Errors, " "), "polecats unreadable") {
		t.Fatalf("errors = %v; want the unreadable listing reported", r.Errors)
	}
	if trim.listed != nil || len(env.reaped) != 0 {
		t.Fatalf("trim asked with %v and reaped %v; want neither", trim.listed, env.reaped)
	}
}

// TestDirCapTrimIgnoresASeatWithoutADirectory: a name the tick did not list in
// the rig has no directory to remove, so it is not a trim seat.
func TestDirCapTrimIgnoresASeatWithoutADirectory(t *testing.T) {
	t.Parallel()
	env := eligible()
	reusableSeat(env)
	r := reapScanner(trimmable(env, "onyx", "gone"), ReapOptions{}).Tick("gastown")
	if _, ok := reapFinding(t, r, "gone"); ok {
		t.Fatalf("findings = %+v; want no finding for a seat with no directory", r.Findings)
	}
	if len(env.reaped) != 1 || env.reaped[0] != "onyx" {
		t.Fatalf("reaped = %v, want [onyx]", env.reaped)
	}
}
