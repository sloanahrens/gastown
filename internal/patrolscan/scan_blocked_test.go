package patrolscan

import (
	"errors"
	"strings"
	"testing"
)

// A recovery's restart decision is the bead's own blocking dependencies over
// the death's confirmation: a seat that may still come back is left alone, an
// unblocked bead is resumed on its preserved branch, and a blocked bead
// releases the seat instead of spawning a session that would defer again
// (gt-otb5m3).
func TestDecideRestart(t *testing.T) {
	t.Parallel()
	const confirmedAt = 2
	open := WorkRef{ID: "gt-blocker", Status: "in_progress"}
	cases := []struct {
		name     string
		blockers []WorkRef
		samples  int
		want     RestartStep
		wantID   string
	}{
		{"unconfirmed death waits, whatever the dependencies", []WorkRef{open}, 1, RestartWait, ""},
		{"confirmed death with no blocker resumes", nil, confirmedAt, RestartResume, ""},
		{"confirmed death with one open blocker releases", []WorkRef{open}, confirmedAt, RestartRelease, "gt-blocker"},
		{
			name:     "several blockers release on the first in ID order",
			blockers: []WorkRef{{ID: "gt-zzz", Status: "open"}, {ID: "gt-aaa", Status: "in_progress"}},
			samples:  confirmedAt,
			want:     RestartRelease,
			wantID:   "gt-aaa",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := DecideRestart(tc.blockers, tc.samples, confirmedAt)
			if plan.Step != tc.want {
				t.Fatalf("step = %v, want %v", plan.Step, tc.want)
			}
			if plan.Blocker.ID != tc.wantID {
				t.Fatalf("blocker = %q, want %q", plan.Blocker.ID, tc.wantID)
			}
		})
	}
}

// The note a deferred bead carries names the blocker and the exit that stopped
// the turn, and says the status was unread rather than printing a gap when the
// dependency read answered none.
func TestDeferredBlockerNote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		blocker WorkRef
		exit    string
		want    []string
	}{
		{
			name:    "the blocker and the exit are named",
			blocker: WorkRef{ID: "gt-blocker", Status: "in_progress"},
			exit:    ExitDeferred,
			want:    []string{"gt-blocker", "in_progress", ExitDeferred, "released instead of restarted"},
		},
		{
			name:    "an unread status is said out loud",
			blocker: WorkRef{ID: "gt-blocker"},
			exit:    ExitEscalated,
			want:    []string{"gt-blocker", "status unread", ExitEscalated},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			note := DeferredBlockerNote(tc.blocker, tc.exit)
			for _, want := range tc.want {
				if !strings.Contains(note, want) {
					t.Errorf("note = %q, want it to name %q", note, want)
				}
			}
		})
	}
}

// A DEFERRED or ESCALATED exit hands the seat's bead back rather than restarting
// the seat when the bead has an unresolved blocker: the nux seat restarted on
// gt-xz6b9v three times on a bead blocked by gt-8r17fp, each turn deferring
// within minutes (gt-otb5m3). An escalation the turn stopped on still wins while
// it is open, and the rows here are the restart decision end to end.
func TestBlockedRecoveryReleasesTheSeat(t *testing.T) {
	t.Parallel()
	deferred := func(env *fakeEnv) {
		deadWithWork(env, "ruby")
		env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred, CleanupStatus: "has_unpushed"}
	}
	settledEscalation := func(env *fakeEnv) {
		stoppedOnEscalation(env, "ruby")
		env.raised = []Escalation{{ID: "hq-wisp-1", EscalatedBy: "gastown/polecats/ruby", Title: "gt-ruby: a decision"}}
	}
	pendingEscalation := func(env *fakeEnv) {
		settledEscalation(env)
		env.raised[0].Open = true
	}
	blocker := []WorkRef{{ID: "gt-blocker", Status: "in_progress"}}

	cases := []struct {
		name     string
		setup    func(env *fakeEnv)
		blockers []WorkRef
		outcome  Outcome
		released bool
		exit     string
	}{
		{name: "DEFERRED exit with an open dependency releases the seat", setup: deferred, blockers: blocker, outcome: OutcomeReleased, released: true, exit: ExitDeferred},
		{name: "DEFERRED exit with no open dependency restarts", setup: deferred, outcome: OutcomeRestarted},
		{name: "ESCALATED exit with its escalation open skips", setup: pendingEscalation, blockers: blocker, outcome: OutcomeSkipped},
		{name: "ESCALATED exit with a closed escalation and an open dependency releases", setup: settledEscalation, blockers: blocker, outcome: OutcomeReleased, released: true, exit: ExitEscalated},
		{name: "ESCALATED exit with a closed escalation and no dependency restarts", setup: settledEscalation, outcome: OutcomeRestarted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			tc.setup(env)
			env.blockers["gt-ruby"] = tc.blockers

			r := scanner(env, nil).Tick("gastown")

			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.outcome {
				t.Fatalf("outcome = %v (%q), want %v", f.Outcome, f.Detail, tc.outcome)
			}
			wantRestart := tc.outcome == OutcomeRestarted
			if got := strings.Join(env.restarts, ",") == "ruby"; got != wantRestart {
				t.Fatalf("restarted = %v, want %v (report %v)", got, wantRestart, r.Lines())
			}
			if got := strings.Join(env.idles, ",") == "ruby"; got != tc.released {
				t.Fatalf("idles = %q, want released = %v", env.idles, tc.released)
			}
			if got := strings.Join(env.deferred, ","); (got != "") != tc.released {
				t.Fatalf("deferred = %q, want released = %v", got, tc.released)
			}
			if !tc.released {
				return
			}
			// The log line names the bead, the blocker and the exit kind.
			for _, want := range []string{"gt-ruby", "gt-blocker", tc.exit} {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("detail = %q, want it to name %q", f.Detail, want)
				}
			}
			// The deferral is guarded on the seat that held the bead, and the
			// note lands with it naming the blocker.
			if got := env.deferred[0]; got != "gt-ruby=gastown/polecats/ruby" {
				t.Errorf("deferral = %q, want the bead deferred from ruby", got)
			}
			if note := env.deferNotes[0]; !strings.Contains(note, "gt-blocker") || !strings.Contains(note, tc.exit) {
				t.Errorf("note = %q, want the blocker and the exit named", note)
			}
		})
	}
}

// A crash — no DEFERRED or ESCALATED exit — on a blocked bead restarts as it
// always did, and the dependency read is not even made: the release is the
// recovery's, and a crashed seat's bead is not the turn's.
func TestCrashOnABlockedBeadRestartsWithoutReadingDependencies(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby") // agent_state working: a crash
	env.blockers["gt-ruby"] = []WorkRef{{ID: "gt-blocker", Status: "in_progress"}}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if len(env.blockerReads) != 0 {
		t.Fatalf("blocking dependencies read for a crash: %v", env.blockerReads)
	}
	if len(env.deferred) != 0 || len(env.idles) != 0 {
		t.Fatalf("deferred = %v, idles = %v; a crash defers nothing", env.deferred, env.idles)
	}
}

// A release that stops halfway reports the step that failed and never passes
// it as a release: the seat is not restarted on a step it could not finish.
func TestBlockedReleaseFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		setup    func(env *fakeEnv)
		outcome  Outcome
		deferred bool
		detail   string
	}{
		{
			name:    "an unreadable dependency leaves the seat hooked",
			setup:   func(env *fakeEnv) { env.blockerErr["gt-ruby"] = errors.New("bd show: connection refused") },
			outcome: OutcomeUnknown,
			detail:  "blocking dependencies unreadable",
		},
		{
			name:    "a failed deferral leaves the seat hooked",
			setup:   func(env *fakeEnv) { env.deferErr["gt-ruby"] = errors.New("bd update: exit status 1") },
			outcome: OutcomeFailed,
			detail:  "deferring gt-ruby",
		},
		{
			name:    "a bead another actor took is left to it",
			setup:   func(env *fakeEnv) { env.deferStale["gt-ruby"] = true },
			outcome: OutcomeSkipped,
			detail:  "moved to another holder",
		},
		{
			name:     "a deferral whose seat release fails is reported",
			setup:    func(env *fakeEnv) { env.idleErr["ruby"] = errors.New("intent: write failed") },
			outcome:  OutcomeFailed,
			deferred: true,
			detail:   "releasing the seat",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			deadWithWork(env, "ruby")
			env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred}
			env.blockers["gt-ruby"] = []WorkRef{{ID: "gt-blocker", Status: "in_progress"}}
			tc.setup(env)

			r := scanner(env, nil).Tick("gastown")

			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.outcome {
				t.Fatalf("outcome = %v (%q), want %v", f.Outcome, f.Detail, tc.outcome)
			}
			if !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to name %q", f.Detail, tc.detail)
			}
			if len(env.restarts) != 0 {
				t.Fatalf("restarts = %v; a blocked seat is never restarted", env.restarts)
			}
			if got := len(env.deferred) > 0; got != tc.deferred {
				t.Fatalf("deferred = %v, want %v", env.deferred, tc.deferred)
			}
		})
	}
}

// A retried release does not append the blocker note twice: the bead's notes
// already name it, so the deferral carries no note and only the guarded status
// change is written.
func TestReleaseDoesNotRepeatTheNote(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred}
	env.blockers["gt-ruby"] = []WorkRef{{ID: "gt-blocker", Status: "in_progress"}}
	env.work["ruby"].Notes = DeferredBlockerNote(env.blockers["gt-ruby"][0], ExitDeferred)

	r := scanner(env, nil).Tick("gastown")

	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeReleased {
		t.Fatalf("outcome = %v (%q), want released", f.Outcome, f.Detail)
	}
	if got := strings.Join(env.deferNotes, "|"); got != "" {
		t.Fatalf("notes written on a retry = %q, want none", got)
	}
}

// A released seat stops being the bead's holder: the bead is deferred to
// nobody, so it is no longer the seat's work and a later tick neither restarts
// the seat nor defers the bead again.
func TestReleaseLeavesTheSeatFreeAndTheBeadDeferred(t *testing.T) {
	t.Parallel()
	env := newFake()
	deadWithWork(env, "ruby")
	env.agents["ruby"] = AgentRecord{State: "stuck", ExitType: ExitDeferred}
	env.blockers["gt-ruby"] = []WorkRef{{ID: "gt-blocker", Status: "in_progress"}}

	first := scanner(env, nil).Tick("gastown")
	if f := seatFinding(t, first, "ruby"); f.Outcome != OutcomeReleased {
		t.Fatalf("first tick outcome = %v (%q), want released", f.Outcome, f.Detail)
	}
	if w := env.work["ruby"]; w.Status != "deferred" || w.Assignee != "" {
		t.Fatalf("bead = %s/%q, want deferred and unassigned", w.Status, w.Assignee)
	}
	if w, err := env.AssignedWork("gastown", "ruby"); err != nil || w != nil {
		t.Fatalf("AssignedWork after the release = %+v, %v; want no work", w, err)
	}

	// Nothing about the seat asks for a session or holds a bead any more, so
	// the next tick has nothing to restart and nothing to defer.
	scanner(env, nil).Tick("gastown")
	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want none across both ticks", env.restarts)
	}
	if len(env.deferred) != 1 {
		t.Fatalf("deferrals = %v, want one across both ticks", env.deferred)
	}
}
