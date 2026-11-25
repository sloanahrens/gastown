package patrolscan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
)

// gt-rwl8bj: TickSeat scans one seat through seat(), so a dead seat that stopped
// on an ESCALATED exit whose escalation is closed is restarted on the tick that
// names it, with a report that covers that seat alone.
func TestTickSeatResumes(t *testing.T) {
	t.Parallel()
	env := newFake()
	stoppedOnEscalation(env, "ruby")
	env.raised = []Escalation{{ID: "hq-wisp-1", EscalatedBy: "gastown/polecats/ruby", Title: "gt-ruby: a decision"}}

	r := scanner(env, nil).TickSeat("gastown", "ruby")

	if r.Checked != 1 {
		t.Fatalf("checked = %d, want 1; report %v", r.Checked, r.Lines())
	}
	if got := strings.Join(env.restarts, ","); got != "ruby" {
		t.Fatalf("restarts = %q, want ruby; report %v", got, r.Lines())
	}
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %d, want 1; report %v", len(r.Findings), r.Lines())
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeRestarted {
		t.Fatalf("outcome = %q, want %q; report %v", f.Outcome, OutcomeRestarted, r.Lines())
	}
}

// traceRead is one call the scanner made on the env. Seat is "" for a read
// that covers the whole rig.
type traceRead struct {
	read, seat string
}

// seatTrace records the reads a tick makes on a seat, and the rig-wide lists, as
// they happen. It wraps the reap-capable fake so the reap pass has an env to run
// against; the methods it overrides answer exactly as the fake does.
type seatTrace struct {
	*fakeReapEnv
	reads []traceRead
}

func (t *seatTrace) rigRead(read string) { t.reads = append(t.reads, traceRead{read: read}) }
func (t *seatTrace) seatRead(read, seat string) {
	t.reads = append(t.reads, traceRead{read: read, seat: seat})
}

func (t *seatTrace) Polecats(rig string) ([]string, error) {
	t.rigRead("Polecats")
	return t.fakeReapEnv.Polecats(rig)
}
func (t *seatTrace) ActiveWork(rig string) ([]Work, error) {
	t.rigRead("ActiveWork")
	return t.fakeReapEnv.ActiveWork(rig)
}
func (t *seatTrace) LeakedWisps(rig string) ([]Wisp, error) {
	t.rigRead("LeakedWisps")
	return t.fakeReapEnv.LeakedWisps(rig)
}
func (t *seatTrace) Intent(rig, p string) (intent.Record, error) {
	t.seatRead("Intent", p)
	return t.fakeReapEnv.Intent(rig, p)
}
func (t *seatTrace) Assess(rig, p string) liveness.Result {
	t.seatRead("Assess", p)
	return t.fakeReapEnv.Assess(rig, p)
}
func (t *seatTrace) AssignedWork(rig, p string) (*Work, error) {
	t.seatRead("AssignedWork", p)
	return t.fakeReapEnv.AssignedWork(rig, p)
}
func (t *seatTrace) AgentRecord(rig, p string) (AgentRecord, error) {
	t.seatRead("AgentRecord", p)
	return t.fakeReapEnv.AgentRecord(rig, p)
}
func (t *seatTrace) Heartbeat(rig, p string) *Heartbeat {
	t.seatRead("Heartbeat", p)
	return t.fakeReapEnv.Heartbeat(rig, p)
}
func (t *seatTrace) SessionExists(rig, p string) (bool, error) {
	t.seatRead("SessionExists", p)
	return t.fakeReapEnv.SessionExists(rig, p)
}
func (t *seatTrace) Recovery(rig, p string) (Recovery, error) {
	t.seatRead("Recovery", p)
	return t.fakeReapEnv.Recovery(rig, p)
}
func (t *seatTrace) IdleSince(rig, p string) (time.Time, error) {
	t.seatRead("IdleSince", p)
	return t.fakeReapEnv.IdleSince(rig, p)
}

// oneSeatEnv is a rig with two seats. ruby is the seat TickSeat is asked about;
// onyx is idle and eligible, so the reap pass a rig-wide tick runs would remove
// it. A tick that reads the rig or runs its passes changes onyx's fate and shows
// up in the trace.
func oneSeatEnv(ruby func(env *fakeEnv)) *seatTrace {
	base := eligible()
	ruby(base.fakeEnv)
	return &seatTrace{fakeReapEnv: base}
}

// TestTickSeatOneSeat pins TickSeat to its one-seat contract: it reads the named
// seat and acts on it through seat(), it never lists the rig or runs the reap,
// orphan or leaked-wisp passes, and a seat with an open escalation is skipped
// while an alive seat with nothing owed reports nothing (gt-dld4ru).
func TestTickSeatOneSeat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ruby     func(env *fakeEnv)
		restarts string
		outcome  Outcome // "" when the seat reports no finding
		detail   string
	}{
		{
			name:     "a dead seat with hooked work is restarted",
			ruby:     func(env *fakeEnv) { deadWithWork(env, "ruby") },
			restarts: "ruby",
			outcome:  OutcomeRestarted,
		},
		{
			name: "an ESCALATED seat whose escalation is open is skipped",
			ruby: func(env *fakeEnv) {
				stoppedOnEscalation(env, "ruby")
				env.raised = []Escalation{{ID: "hq-wisp-1", EscalatedBy: "gastown/polecats/ruby", Title: "gt-ruby: a decision", Open: true}}
			},
			outcome: OutcomeSkipped,
			detail:  "hq-wisp-1 is still open",
		},
		{
			name: "an alive seat with nothing owed reports nothing",
			ruby: func(env *fakeEnv) {
				env.polecats = append(env.polecats, "ruby")
				env.work["ruby"] = &Work{ID: "gt-ruby", Status: "hooked", Assignee: "gastown/polecats/ruby", UpdatedAt: now.Add(-time.Hour)}
				env.agents["ruby"] = AgentRecord{State: "working"}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := oneSeatEnv(tc.ruby)
			r := tickSeatScanner(env).TickSeat("gastown", "ruby")

			if r.Checked != 1 {
				t.Errorf("checked = %d, want 1; report %v", r.Checked, r.Lines())
			}
			for _, rd := range env.reads {
				if rd.seat == "" {
					t.Errorf("rig-wide read %s; a one-seat tick must not list the rig; report %v", rd.read, r.Lines())
				} else if rd.seat != "ruby" {
					t.Errorf("%s read seat %q; want only ruby", rd.read, rd.seat)
				}
			}
			if got := strings.Join(env.restarts, ","); got != tc.restarts {
				t.Errorf("restarts = %q, want %q; report %v", got, tc.restarts, r.Lines())
			}
			if len(env.reaped) != 0 {
				t.Errorf("reaped = %v; the reap pass must not run", env.reaped)
			}
			if tc.outcome == "" {
				if len(r.Findings) != 0 {
					t.Fatalf("findings = %d, want none; report %v", len(r.Findings), r.Lines())
				}
				return
			}
			if len(r.Findings) != 1 {
				t.Fatalf("findings = %d, want 1; report %v", len(r.Findings), r.Lines())
			}
			f := seatFinding(t, r, "ruby")
			if f.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q; report %v", f.Outcome, tc.outcome, r.Lines())
			}
			if tc.detail != "" && !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", f.Detail, tc.detail)
			}
		})
	}
}

// tickSeatScanner is the scanner TickSeat runs under in the tests above: the
// options scanner() gives, with the reap pass on so a rig-wide tick would run it.
func tickSeatScanner(env *seatTrace) *Scanner {
	return New(env, memLedger{}, Options{
		Now:       func() time.Time { return now },
		IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		SeatCap:   env.fakeEnv.seatCapFor,
		Reap:      &ReapOptions{MaxPerTick: 5},
	})
}
