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
