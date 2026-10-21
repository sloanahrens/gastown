package patrolscan

import (
	"errors"
	"testing"
)

// The escalation tie is read in three places (the stuck-seat tick, the daemon's
// crash detector and the overrun guard), so its answers are pinned here
// directly, not only through a whole tick.

const tieRig, tieName, tieBead = "gastown", "ruby", "gt-ruby"

func TestSeatEscalationVerdictAnswers(t *testing.T) {
	t.Parallel()
	const seat = "gastown/polecats/ruby"
	cases := []struct {
		name    string
		raised  []Escalation
		verdict EscalationVerdict
		id      string
	}{
		{"no escalations at all", nil, EscalationUnnamed, ""},
		{"closed, named by related_bead",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, RelatedBead: tieBead}},
			EscalationResolved, "hq-1"},
		{"closed, named by the title prefix",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, Title: "gt-ruby: a decision"}},
			EscalationResolved, "hq-1"},
		{"bead named in a different case",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, Title: "GT-RUBY: a decision"}},
			EscalationResolved, "hq-1"},
		{"raiser address differs only in case and padding",
			[]Escalation{{ID: "hq-1", EscalatedBy: " Gastown/Polecats/RUBY ", RelatedBead: tieBead}},
			EscalationResolved, "hq-1"},
		{"open, named by the title prefix",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, Title: "gt-ruby: a decision", Open: true}},
			EscalationPending, "hq-1"},
		{"open beside a closed one: open holds the seat",
			[]Escalation{
				{ID: "hq-1", EscalatedBy: seat, RelatedBead: tieBead},
				{ID: "hq-2", EscalatedBy: seat, RelatedBead: tieBead, Open: true},
			},
			EscalationPending, "hq-2"},
		{"two closed: the lowest ID answers, whatever the listing order",
			[]Escalation{
				{ID: "hq-9", EscalatedBy: seat, RelatedBead: tieBead},
				{ID: "hq-2", EscalatedBy: seat, RelatedBead: tieBead},
			},
			EscalationResolved, "hq-2"},
		{"title mentions the bead without naming it",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, Title: "escalating about gt-ruby"}},
			EscalationUnnamed, ""},
		{"title starts with the bead but no colon follows",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, Title: "gt-ruby a decision"}},
			EscalationUnnamed, ""},
		{"title prefix is a longer bead id",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, Title: "gt-ruby2: a decision"}},
			EscalationUnnamed, ""},
		{"names another bead",
			[]Escalation{{ID: "hq-1", EscalatedBy: seat, RelatedBead: "gt-other"}},
			EscalationUnnamed, ""},
		{"another role's escalation for the bead",
			[]Escalation{{ID: "hq-1", EscalatedBy: "gastown/crew/sloan", RelatedBead: tieBead}},
			EscalationUnnamed, ""},
		{"same polecat name in another rig",
			[]Escalation{{ID: "hq-1", EscalatedBy: "beads/polecats/ruby", RelatedBead: tieBead}},
			EscalationUnnamed, ""},
		{"open escalation of another seat",
			[]Escalation{{ID: "hq-1", EscalatedBy: "gastown/polecats/pearl", RelatedBead: tieBead, Open: true}},
			EscalationUnnamed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SeatEscalationVerdict(tieRig, tieName, tieBead, tc.raised)
			if got.Verdict != tc.verdict {
				t.Fatalf("verdict = %q (%q), want %q", got.Verdict, got.Detail, tc.verdict)
			}
			if got.ID != tc.id {
				t.Errorf("id = %q, want %q", got.ID, tc.id)
			}
			// Detail says why a seat is held, so it is set exactly when held.
			if held := got.Verdict != EscalationResolved; held != (got.Detail != "") {
				t.Errorf("detail = %q for verdict %q; detail must be set exactly when the seat is held", got.Detail, got.Verdict)
			}
		})
	}
}

func TestSeatEscalationVerdictEmptyBeadNamesNothing(t *testing.T) {
	t.Parallel()
	raised := []Escalation{{ID: "hq-1", EscalatedBy: "gastown/polecats/ruby", RelatedBead: "", Title: ": a decision"}}
	got := SeatEscalationVerdict(tieRig, tieName, "  ", raised)
	if got.Verdict != EscalationUnnamed {
		t.Fatalf("verdict = %q, want %q: an empty bead is named by nothing", got.Verdict, EscalationUnnamed)
	}
}

func TestSeatEscalationHoldAnswers(t *testing.T) {
	t.Parallel()
	const seat = "gastown/polecats/ruby"
	open := []Escalation{{ID: "hq-1", EscalatedBy: seat, RelatedBead: tieBead, Open: true}}
	closed := []Escalation{{ID: "hq-1", EscalatedBy: seat, RelatedBead: tieBead}}
	cases := []struct {
		name    string
		rec     AgentRecord
		raised  []Escalation
		verdict EscalationVerdict
	}{
		{"working seat that raised an open escalation is not held",
			AgentRecord{State: "working", ExitType: ExitEscalated}, open, EscalationNotStopped},
		{"stuck for a DEFERRED exit is not an escalation stop",
			AgentRecord{State: "stuck", ExitType: ExitDeferred}, open, EscalationNotStopped},
		{"no exit recorded",
			AgentRecord{State: "stuck"}, open, EscalationNotStopped},
		{"stopped on an escalation, still open",
			AgentRecord{State: "stuck", ExitType: ExitEscalated}, open, EscalationPending},
		{"stopped on an escalation, closed",
			AgentRecord{State: "stuck", ExitType: ExitEscalated}, closed, EscalationResolved},
		{"stopped on an escalation, none raised for the bead",
			AgentRecord{State: "stuck", ExitType: ExitEscalated}, nil, EscalationUnnamed},
		{"record fields in a different case and padding",
			AgentRecord{State: " STUCK ", ExitType: " escalated "}, open, EscalationPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			got, err := SeatEscalationHold(tc.rec, tieRig, tieName, tieBead, func() ([]Escalation, error) {
				reads++
				return tc.raised, nil
			})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.Verdict != tc.verdict {
				t.Fatalf("verdict = %q (%q), want %q", got.Verdict, got.Detail, tc.verdict)
			}
			// Escalations are read only for a seat stopped on one.
			if wantRead := tc.verdict != EscalationNotStopped; wantRead != (reads == 1) {
				t.Errorf("escalations read %d times; want read = %v", reads, wantRead)
			}
			if tc.verdict == EscalationNotStopped && got.Detail != "" {
				t.Errorf("not-stopped detail = %q, want empty", got.Detail)
			}
		})
	}
}

func TestSeatEscalationHoldReadFailureIsReturned(t *testing.T) {
	t.Parallel()
	readErr := errors.New("bd list escalations: connection refused")
	rec := AgentRecord{State: "stuck", ExitType: ExitEscalated}
	got, err := SeatEscalationHold(rec, tieRig, tieName, tieBead, func() ([]Escalation, error) {
		return nil, readErr
	})
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want the read failure", err)
	}
	if got.Verdict != "" {
		t.Errorf("verdict = %q on a failed read, want none", got.Verdict)
	}
}
