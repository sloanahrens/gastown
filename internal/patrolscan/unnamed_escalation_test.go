package patrolscan

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// filerEnv is a fakeEnv that also files the missing escalation, recording each call.
type filerEnv struct {
	*fakeEnv
	filed []string
	err   error
}

func (f *filerEnv) FileUnnamedEscalation(rig, polecat, bead string) error {
	f.filed = append(f.filed, rig+"/"+polecat+"/"+bead)
	return f.err
}

// unnamedStop is a dead seat that stopped on ESCALATED and whose agent record was last written
// `age` before now, with no escalation of the seat's naming its bead.
func unnamedStop(age time.Duration) *fakeEnv {
	env := newFake()
	stoppedOnEscalation(env, "ruby")
	rec := env.agents["ruby"]
	if age >= 0 {
		rec.UpdatedAt = now.Add(-age)
	}
	env.agents["ruby"] = rec
	return env
}

// TestUnnamedEscalationFiledAfterTheGrace: a seat that stopped on ESCALATED with no escalation
// naming its bead is reported to the overseer once its record is older than the grace; the seat
// stays skipped either way, a filer error is in the detail, and a record inside the grace or
// with no write time files nothing (gt-7046im).
func TestUnnamedEscalationFiledAfterTheGrace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		age    time.Duration // the agent record's age; negative leaves UpdatedAt zero
		err    error
		filed  int
		detail string
	}{
		{"older than the grace files one", unnamedEscalationGrace + time.Minute, nil, 1, "filed one for the overseer"},
		{"exactly at the grace files one", unnamedEscalationGrace, nil, 1, "filed one for the overseer"},
		{"inside the grace files nothing", unnamedEscalationGrace - time.Second, nil, 0, "no escalation of the seat's names gt-ruby"},
		{"no write time files nothing", -1, nil, 0, "no escalation of the seat's names gt-ruby"},
		{"a filer error is in the detail", unnamedEscalationGrace + time.Minute, errors.New("gt escalate: refused"), 1, "filing one failed: gt escalate: refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := &filerEnv{fakeEnv: unnamedStop(c.age), err: c.err}
			r := New(env, memLedger{}, Options{Now: func() time.Time { return now }}).Tick("gastown")
			if len(env.filed) != c.filed || (c.filed == 1 && env.filed[0] != "gastown/ruby/gt-ruby") {
				t.Errorf("filed = %v, want %d call for gastown/ruby/gt-ruby", env.filed, c.filed)
			}
			f := seatFinding(t, r, "ruby")
			if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, c.detail) {
				t.Errorf("finding = %v %q, want skipped naming %q", f.Outcome, f.Detail, c.detail)
			}
			if len(env.restarts) != 0 {
				t.Errorf("restarts = %v, want none: the seat stays skipped", env.restarts)
			}
		})
	}
}

// TestUnnamedEscalationLeavesTheOtherVerdictsAlone: an escalation that names the bead, open or
// closed, and a host without the filer leave the rule uncalled and today's skip or restart as
// it was (gt-7046im).
func TestUnnamedEscalationLeavesTheOtherVerdictsAlone(t *testing.T) {
	t.Parallel()
	old := unnamedEscalationGrace + time.Hour
	named := func(open bool) []Escalation {
		return []Escalation{{ID: "hq-1", EscalatedBy: "gastown/polecats/ruby", Title: "gt-ruby: a decision", Open: open}}
	}
	t.Run("an open escalation holds the seat and files nothing", func(t *testing.T) {
		t.Parallel()
		env := &filerEnv{fakeEnv: unnamedStop(old)}
		env.raised = named(true)
		r := New(env, memLedger{}, Options{Now: func() time.Time { return now }}).Tick("gastown")
		if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeSkipped || len(env.filed) != 0 {
			t.Errorf("outcome %v, filed %v; want skipped and nothing filed", f.Outcome, env.filed)
		}
	})
	t.Run("a closed escalation resumes the seat and files nothing", func(t *testing.T) {
		t.Parallel()
		env := &filerEnv{fakeEnv: unnamedStop(old)}
		env.raised = named(false)
		New(env, memLedger{}, Options{Now: func() time.Time { return now }}).Tick("gastown")
		if len(env.restarts) != 1 || len(env.filed) != 0 {
			t.Errorf("restarts %v, filed %v; want one restart and nothing filed", env.restarts, env.filed)
		}
	})
	t.Run("a host without the filer keeps today's skip", func(t *testing.T) {
		t.Parallel()
		env := unnamedStop(old)
		r := scanner(env, nil).Tick("gastown")
		f := seatFinding(t, r, "ruby")
		if f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "no escalation of the seat's names gt-ruby") {
			t.Errorf("finding = %v %q, want today's skip", f.Outcome, f.Detail)
		}
	})
}

// failingLedger is a ledger whose marks fail, to see the filing reported and tried again.
type failingLedger struct{ memLedger }

func (failingLedger) MarkReported(string, time.Time) error { return errors.New("ledger: disk full") }

func unnamedTick(env *filerEnv, ledger Ledger) Report {
	return New(env, ledger, Options{Now: func() time.Time { return now }}).Tick("gastown")
}

// TestUnnamedEscalationIsFiledOncePerExit: after a successful filing the filer is not called on
// the next ticks (the seat stays unnamed, since the daemon raised the escalation, not the seat),
// the seat is skipped as filed already, and a later ESCALATED exit (a newer record write) files
// again (gt-5wmwv4).
func TestUnnamedEscalationIsFiledOncePerExit(t *testing.T) {
	t.Parallel()
	env := &filerEnv{fakeEnv: unnamedStop(unnamedEscalationGrace + time.Minute)}
	ledger := memLedger{}

	unnamedTick(env, ledger)
	unnamedTick(env, ledger)
	r := unnamedTick(env, ledger)

	if len(env.filed) != 1 {
		t.Fatalf("filed = %v after three ticks, want exactly one call", env.filed)
	}
	if f := seatFinding(t, r, "ruby"); f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "was filed already") {
		t.Errorf("third tick: %q %q, want skipped as filed already", f.Outcome, f.Detail)
	}

	rec := env.agents["ruby"]
	rec.UpdatedAt = now.Add(-unnamedEscalationGrace - time.Second) // a later exit than the one filed for
	env.agents["ruby"] = rec
	unnamedTick(env, ledger)
	if len(env.filed) != 2 {
		t.Errorf("filed = %v after a later exit, want a second call", env.filed)
	}
}

// TestUnnamedEscalationFailuresAreNotMarked: a failed filing marks nothing, so the next tick
// calls the filer again with the failure in the detail; a mark that fails is named in the detail
// and the next tick calls the filer again too (gt-5wmwv4).
func TestUnnamedEscalationFailuresAreNotMarked(t *testing.T) {
	t.Parallel()
	t.Run("a failed filing", func(t *testing.T) {
		t.Parallel()
		env := &filerEnv{fakeEnv: unnamedStop(unnamedEscalationGrace + time.Minute), err: errors.New("gt escalate: refused")}
		ledger := memLedger{}
		unnamedTick(env, ledger)
		r := unnamedTick(env, ledger)
		if len(env.filed) != 2 {
			t.Errorf("filed = %v, want a call on each tick while the failure stands", env.filed)
		}
		if f := seatFinding(t, r, "ruby"); !strings.Contains(f.Detail, "filing one failed: gt escalate: refused") {
			t.Errorf("detail = %q, want the failure named every tick", f.Detail)
		}
		if len(ledger) != 0 {
			t.Errorf("ledger = %v, want nothing marked for a failed filing", ledger)
		}
	})
	t.Run("a mark that fails", func(t *testing.T) {
		t.Parallel()
		env := &filerEnv{fakeEnv: unnamedStop(unnamedEscalationGrace + time.Minute)}
		ledger := failingLedger{memLedger{}}
		unnamedTick(env, ledger)
		r := unnamedTick(env, ledger)
		if len(env.filed) != 2 {
			t.Errorf("filed = %v, want the filer called again while the mark cannot be written", env.filed)
		}
		if f := seatFinding(t, r, "ruby"); !strings.Contains(f.Detail, "remembering it failed: ledger: disk full") {
			t.Errorf("detail = %q, want the failed mark named", f.Detail)
		}
	})
}
