package patrolscan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
)

// liveQuiet sets polecat p up as a live seat working bead gt-<p>, whose
// progress evidence has stood still for quiet.
func liveQuiet(env *fakeEnv, p string, quiet time.Duration) {
	env.polecats = []string{p}
	env.verdicts[p] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: quiet}
	env.work[p] = &Work{ID: "gt-" + p, Status: "in_progress", Assignee: "gastown/polecats/" + p, UpdatedAt: now.Add(-time.Hour)}
}

// reviveScanner is scanner() with a clock the test can move, for the ticks that
// must happen minutes apart.
func reviveScanner(env *fakeEnv, at *time.Time) *Scanner {
	return New(env, memLedger{}, Options{
		Now:       func() time.Time { return *at },
		IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		SeatCap:   env.seatCapFor,
	})
}

// gt-k6t2b3: a live seat that pushed its branch and then went quiet before gt
// done has finished its work and lost its turn — after a dropped API
// connection the pane sits at an empty prompt, and a nudge cannot reach a
// sandbox seat. The tick restarts it onto the pushed branch, names the bead,
// the head and the quiet time in the reason, and records its cooldown stamp.
func TestRevivePushedSeatIsRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeRevived, r.Lines())
	}
	for _, want := range []string{"gt-ruby", "abc1234", "6m0s"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
	if line := r.Lines()[0]; !strings.Contains(line, "1 revived") {
		t.Errorf("summary = %q, want the revival counted", line)
	}
	if got, ok := env.revived["ruby=gt-ruby"]; !ok || !got.Equal(now) {
		t.Errorf("cooldown stamp = %v (present %v), want %v", got, ok, now)
	}
}

// A stalled seat has stood still longer still, and the rule fires before the
// stall is reported: a stall on a pushed branch is finished work too.
func TestReviveStalledSeatOnAPushedBranch(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 31*time.Minute)
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Stalled, Reason: "no progress for 31m", QuietFor: 31 * time.Minute}
	env.pushed["ruby"] = "abc1234"

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeRevived, r.Lines())
	}
}

// Every other shape of a quiet live seat is left exactly as it was: too short
// a quiet, nothing pushed since the sling, work that is submitted or held or
// not assigned, a parked seat, a heartbeat that says stuck or is inside gt
// done, a revival still inside its cooldown, and any read or write that fails.
func TestRevivePushedSeatIsLeftAlone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(*fakeEnv)
		want   Outcome // "" = no finding
		detail string
	}{
		{"quiet 3 minutes", func(env *fakeEnv) {
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: 3 * time.Minute}
		}, "", ""},
		{"nothing pushed since the sling", func(env *fakeEnv) { env.pushed["ruby"] = "" }, "", ""},
		{"work submitted for landing", func(env *fakeEnv) {
			env.work["ruby"].Labels = []string{ReadyToLandLabel}
		}, "", ""},
		{"work held from dispatch", func(env *fakeEnv) {
			env.work["ruby"].Labels = []string{"needs-human"}
		}, "", ""},
		{"work status is not an assigned one", func(env *fakeEnv) {
			env.work["ruby"].Status = "blocked"
		}, "", ""},
		{"work is closed", func(env *fakeEnv) { env.work["ruby"].Status = "closed" }, "", ""},
		{"seat parked", func(env *fakeEnv) {
			env.intents["ruby"] = intent.Record{Desired: intent.DesiredPark}
		}, "", ""},
		{"heartbeat says stuck", func(env *fakeEnv) {
			env.heartbeats["ruby"] = &Heartbeat{State: "stuck", At: now.Add(-6 * time.Minute)}
		}, OutcomeSkipped, "heartbeat says stuck"},
		{"heartbeat says exiting inside gt done", func(env *fakeEnv) {
			env.heartbeats["ruby"] = &Heartbeat{State: "exiting", At: now.Add(-time.Minute)}
		}, OutcomeSkipped, "inside gt done"},
		{"revived inside the cooldown", func(env *fakeEnv) {
			env.revived["ruby=gt-ruby"] = now.Add(-10 * time.Minute)
		}, OutcomeSkipped, "cooldown holds it"},
		{"pushed read fails", func(env *fakeEnv) {
			env.pushedErr["ruby"] = errors.New("git: bad object")
		}, OutcomeUnknown, "pushed branch unreadable"},
		{"work read fails", func(env *fakeEnv) {
			env.workErr["ruby"] = errors.New("bd list: connection refused")
		}, OutcomeUnknown, "work unreadable"},
		{"cooldown read fails", func(env *fakeEnv) {
			env.reviveReadErr["ruby=gt-ruby"] = errors.New("intent record unreadable")
		}, OutcomeUnknown, "cooldown unreadable"},
		{"recording the revival fails", func(env *fakeEnv) {
			env.markRevivedErr["ruby=gt-ruby"] = errors.New("intent lock: timed out")
		}, OutcomeUnknown, "recording the revival"},
		{"liveness unknown", func(env *fakeEnv) {
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Unknown, Err: errors.New("tmux: no server")}
		}, OutcomeUnknown, "liveness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			liveQuiet(env, "ruby", 6*time.Minute)
			env.pushed["ruby"] = "abc1234"
			tc.setup(env)
			before, hadBefore := env.revived["ruby=gt-ruby"]

			r := scanner(env, nil).Tick("gastown")

			if len(env.restarts) != 0 {
				t.Fatalf("restarted %v; want none. report: %v", env.restarts, r.Lines())
			}
			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q. report: %v", f.Outcome, tc.want, r.Lines())
			}
			if tc.detail != "" && !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to name %q", f.Detail, tc.detail)
			}
			if after, ok := env.revived["ruby=gt-ruby"]; ok != hadBefore || !after.Equal(before) {
				t.Errorf("cooldown stamp changed from %v (present %v) to %v (present %v) for a seat that was not revived", before, hadBefore, after, ok)
			}
		})
	}
}

// The supervisor refuses a restart for a held or e-stopped seat, and the
// refusal is reported as it is for every other restart of the seat. The stamp
// written before the attempt stays, so the refused restart is not retried on
// every tick.
func TestRevivePushedSeatRefusedByTheSupervisor(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"
	env.restartErr["ruby"] = errRefused

	r := scanner(env, nil).Tick("gastown")

	if got := seatFinding(t, r, "ruby").Outcome; got != OutcomeRefused {
		t.Fatalf("outcome = %q, want %q. report: %v", got, OutcomeRefused, r.Lines())
	}
	if _, ok := env.revived["ruby=gt-ruby"]; !ok {
		t.Errorf("the refused attempt left no cooldown stamp; it would be retried every tick")
	}
}

// A seat revived and quiet again inside the 30-minute cooldown is reported and
// not restarted again, so a seat that genuinely cannot finish does not loop.
// The cooldown is the stamp the scan wrote before the restart, so a later
// supervisor action on the seat cannot erase it.
func TestReviveQuietAgainInsideTheCooldownIsReportedNotRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"
	at := now
	s := reviveScanner(env, &at)

	if r := s.Tick("gastown"); len(env.restarts) != 1 {
		t.Fatalf("restarts = %v, want the one revival; report %v", env.restarts, r.Lines())
	}

	// Ten minutes on, the revived session is quiet again, its branch is still
	// the pushed one, and the supervisor has since written other actions on
	// the seat. The cooldown survives them.
	at = now.Add(10 * time.Minute)
	rec := env.intents["ruby"]
	rec.LastAction = nil
	rec.Restarts = append(rec.Restarts, now.Add(2*time.Minute))
	env.intents["ruby"] = rec
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: 11 * time.Minute}
	r := s.Tick("gastown")

	if len(env.restarts) != 1 {
		t.Fatalf("restarts = %v, want no second restart inside the cooldown; report %v", env.restarts, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeSkipped, r.Lines())
	}
	for _, want := range []string{"gt-ruby", "10m0s", "30m0s"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
}

// The cooldown is per bead: the seat's revival of another bead does not hold
// this one.
func TestReviveCooldownIsPerBead(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"
	env.revived["ruby=gt-other"] = now.Add(-10 * time.Minute)

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if got := seatFinding(t, r, "ruby").Outcome; got != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", got, OutcomeRevived, r.Lines())
	}
}

// The cooldown ends at 30 minutes: a stamp older than that does not hold the
// seat, so a seat that lost its turn again is revived again.
func TestReviveCooldownExpiresAfterThirtyMinutes(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"
	env.revived["ruby=gt-ruby"] = now.Add(-31 * time.Minute)

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if got := seatFinding(t, r, "ruby").Outcome; got != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", got, OutcomeRevived, r.Lines())
	}
}

// A restart that failed after the stamp was written is not retried every tick:
// the first tick reports the failure and the cooldown holds the next one.
func TestReviveFailedRestartSpendsTheCooldown(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"
	restartErr := errors.New("tmux: server exited")
	env.restartErr["ruby"] = restartErr
	at := now
	s := reviveScanner(env, &at)

	r := s.Tick("gastown")

	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeFailed, r.Lines())
	}
	if !strings.Contains(f.Detail, restartErr.Error()) {
		t.Errorf("detail = %q, want it to carry %q", f.Detail, restartErr)
	}
	if _, ok := env.revived["ruby=gt-ruby"]; !ok {
		t.Fatalf("a failed restart left no cooldown stamp; the next tick would retry it")
	}

	at = now.Add(10 * time.Minute)
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: 16 * time.Minute}
	r = s.Tick("gastown")

	if len(env.restarts) != 0 {
		t.Fatalf("restarts = %v, want no retry inside the cooldown; report %v", env.restarts, r.Lines())
	}
	if got := seatFinding(t, r, "ruby").Outcome; got != OutcomeSkipped {
		t.Fatalf("outcome = %q, want %q; report %v", got, OutcomeSkipped, r.Lines())
	}
}

// The exiting heartbeat only holds the seat while it is fresh: past
// HeartbeatFresh it is the previous session's and the revival goes ahead.
func TestReviveStaleExitingHeartbeatDoesNotHoldTheSeat(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 6*time.Minute)
	env.pushed["ruby"] = "abc1234"
	env.heartbeats["ruby"] = &Heartbeat{State: "exiting", At: now.Add(-10 * time.Minute)}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby on a stale exiting heartbeat; report %v", got, r.Lines())
	}
	if got := seatFinding(t, r, "ruby").Outcome; got != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", got, OutcomeRevived, r.Lines())
	}
}

// answeredEscalation is an escalation ruby raised for its open bead gt-ruby,
// closed closedAgo before now, with a close reason the restart must carry.
func answeredEscalation(id string, closedAgo time.Duration) Escalation {
	return Escalation{ID: id, EscalatedBy: "gastown/polecats/ruby", RelatedBead: "gt-ruby",
		CloseReason: "use the staging key", CreatedAt: now.Add(-30 * time.Minute), ClosedAt: now.Add(-closedAgo)}
}

// gt-r3c58b: a live seat that stopped on an escalation of its own for its open
// bead, and then went quiet, is waiting on an answer its session never read: a
// sandbox seat cannot be nudged, and the close nudge only reaches a seat that
// saw the escalation open. The escalation was closed after the seat's last
// output, so the tick restarts the seat, whether or not it pushed, names the
// escalation and its close reason, and stamps the revival cooldown.
func TestReviveAnsweredSeatIsRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 10*time.Minute)
	env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute)}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeRevived, r.Lines())
	}
	for _, want := range []string{"gt-ruby", "hq-7", "use the staging key"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", f.Detail, want)
		}
	}
	if got, ok := env.revived["ruby=gt-ruby"]; !ok || !got.Equal(now) {
		t.Errorf("cooldown stamp = %v (present %v), want %v", got, ok, now)
	}
}

// Every other shape of an answered-looking seat is left alone: an escalation
// closed before the seat's last output (it answered itself, or kept working),
// one closed under the two-minute grace, one still open beside a closed one, a
// submitted bead, a heartbeat that says stuck, and a seat with no escalation.
func TestReviveAnsweredSeatIsLeftAlone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(*fakeEnv)
		want   Outcome // "" = no finding
		detail string
	}{
		{"escalation closed before the last output", func(env *fakeEnv) {
			env.raised = []Escalation{answeredEscalation("hq-7", 20*time.Minute)}
		}, "", ""},
		{"escalation closed 1 minute ago", func(env *fakeEnv) {
			env.raised = []Escalation{answeredEscalation("hq-7", time.Minute)}
		}, OutcomeSkipped, "close nudge"},
		{"open beside a closed one", func(env *fakeEnv) {
			open := answeredEscalation("hq-8", 0)
			open.ClosedAt, open.Open = time.Time{}, true
			env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute), open}
		}, "", ""},
		{"closed with no close time beside a closed one", func(env *fakeEnv) {
			unclosed := answeredEscalation("hq-8", 0)
			unclosed.ClosedAt = time.Time{}
			env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute), unclosed}
		}, OutcomeSkipped, "no close time"},
		{"work submitted for landing", func(env *fakeEnv) {
			env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute)}
			env.work["ruby"].Labels = []string{ReadyToLandLabel}
		}, "", ""},
		{"heartbeat says stuck", func(env *fakeEnv) {
			env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute)}
			env.heartbeats["ruby"] = &Heartbeat{State: "stuck", At: now.Add(-6 * time.Minute)}
		}, OutcomeSkipped, "heartbeat says stuck"},
		{"no escalation of the seat's names the bead", func(env *fakeEnv) {
			e := answeredEscalation("hq-7", 6*time.Minute)
			e.RelatedBead = "gt-other"
			env.raised = []Escalation{e}
		}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			liveQuiet(env, "ruby", 10*time.Minute)
			tc.setup(env)

			r := scanner(env, nil).Tick("gastown")

			if len(env.restarts) != 0 {
				t.Fatalf("restarts = %v, want none; report %v", env.restarts, r.Lines())
			}
			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, tc.want, r.Lines())
			}
			if tc.detail != "" && !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to name %q", f.Detail, tc.detail)
			}
		})
	}
}

// Unmeasured answers act on nothing: an escalation list that cannot be read, and
// a liveness read that failed, are reported unknown and never restart the seat.
func TestReviveAnsweredUnmeasuredActsOnNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(*fakeEnv)
		outcome Outcome
		detail  string
	}{
		{"escalation list unreadable", func(env *fakeEnv) {
			env.raisedErr = errors.New("bd list escalations: connection refused")
		}, OutcomeUnknown, "escalations unreadable"},
		{"liveness unknown", func(env *fakeEnv) {
			env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute)}
			env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Unknown, Err: errors.New("tmux: no server")}
		}, OutcomeUnknown, "liveness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			liveQuiet(env, "ruby", 10*time.Minute)
			tc.setup(env)

			r := scanner(env, nil).Tick("gastown")

			if len(env.restarts) != 0 {
				t.Fatalf("restarts = %v, want none; report %v", env.restarts, r.Lines())
			}
			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.outcome || !strings.Contains(f.Detail, tc.detail) {
				t.Fatalf("finding = %q %q, want %q with %q; report %v", f.Outcome, f.Detail, tc.outcome, tc.detail, r.Lines())
			}
		})
	}
}

// A second firing inside the cooldown is reported and not restarted: the seat
// was revived for the answer ten minutes ago, and the cooldown stamp holds it.
func TestReviveAnsweredInsideTheCooldownIsReportedNotRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 10*time.Minute)
	env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute)}
	at := now
	s := reviveScanner(env, &at)

	if r := s.Tick("gastown"); len(env.restarts) != 1 {
		t.Fatalf("restarts = %v, want the one revival; report %v", env.restarts, r.Lines())
	}

	at = now.Add(10 * time.Minute)
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: 20 * time.Minute}
	r := s.Tick("gastown")

	if len(env.restarts) != 1 {
		t.Fatalf("restarts = %v, want no second restart inside the cooldown; report %v", env.restarts, r.Lines())
	}
	f := seatFinding(t, r, "ruby")
	if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "cooldown holds") {
		t.Fatalf("finding = %q %q, want skipped by the cooldown; report %v", f.Outcome, f.Detail, r.Lines())
	}
}

// A stall after an answered escalation is the same waiting seat, only quieter,
// so the rule fires before the stall is reported.
func TestReviveAnsweredStalledSeatIsRestarted(t *testing.T) {
	t.Parallel()
	env := newFake()
	liveQuiet(env, "ruby", 31*time.Minute)
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Stalled, Reason: "no progress for 31m", QuietFor: 31 * time.Minute}
	env.raised = []Escalation{answeredEscalation("hq-7", 6*time.Minute)}

	r := scanner(env, nil).Tick("gastown")

	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRevived {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeRevived, r.Lines())
	}
}
