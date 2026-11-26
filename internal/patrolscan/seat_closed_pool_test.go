package patrolscan

import (
	"errors"
	"strings"
	"testing"
)

// gt-14qk4q: a recovery restart is not held by the pool cap (gt-02tcp), but it
// names the pool it resumes on. An answered seat put back to work on a pool the
// operator closed used to read as an ordinary restart; the note says which
// pool it resumed on, an open pool says nothing, and a cap read that fails is
// named without stopping the restart.
func TestSeatResumeOnClosedPool(t *testing.T) {
	t.Parallel()
	const key = "polecat_pool.seats.deepseek-flash.cap"
	const reason = "patrol scan: no session with gt-ruby hooked after an ESCALATED exit (escalation hq-wisp-1 closed, 2 dead samples)"
	errRead := errors.New("reading town settings: permission denied")
	cases := []struct {
		name   string
		cap    SeatCap
		capErr error
		want   string // the whole finding detail
	}{
		{
			name: "closed pool names the pool",
			cap:  SeatCap{Live: 1, Closed: true, Key: key},
			want: reason + "; resumed on closed pool " + key + " (cap 0)",
		},
		{
			name: "open pool leaves the detail unchanged",
			cap:  SeatCap{Live: 1, Limit: 3, Key: "polecat_pool.max_seats"},
			want: reason,
		},
		{
			name:   "unreadable cap still restarts and names the read error",
			capErr: errRead,
			want:   reason + "; cap read failed: " + errRead.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newFake()
			stoppedOnEscalation(env, "ruby")
			env.raised = []Escalation{{
				ID: "hq-wisp-1", EscalatedBy: "gastown/polecats/ruby", Title: "gt-ruby: a decision",
			}}
			env.seatCap, env.seatCapErr = tc.cap, tc.capErr

			r := scanner(env, nil).Tick("gastown")

			if got := strings.Join(env.restarts, ","); got != "ruby" {
				t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
			}
			f := seatFinding(t, r, "ruby")
			if f.Outcome != OutcomeRestarted {
				t.Fatalf("outcome = %v (%q), want restarted", f.Outcome, f.Detail)
			}
			if f.Detail != tc.want {
				t.Fatalf("detail = %q, want %q", f.Detail, tc.want)
			}
		})
	}
}
