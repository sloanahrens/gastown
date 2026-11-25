package patrolscan

import (
	"strings"
	"testing"
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
