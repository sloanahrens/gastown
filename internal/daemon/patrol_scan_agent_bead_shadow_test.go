package daemon

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/seatfacts"
)

// agentBeadRetireBead is the agent bead every row of the shadow table reads:
// a finished turn whose state, exit and cleanup the intent record can be held
// against.
var agentBeadRetireBead = patrolscan.AgentRecord{
	State:           "stuck",
	ExitType:        "DEFERRED",
	CleanupStatus:   "has_unpushed",
	HookBead:        "gt-hook",
	LastSourceIssue: "gt-last",
}

// agentBeadRetireFields is the same bead in the fields the fake beads database
// answers AgentRecord with.
var agentBeadRetireFields = beads.AgentFields{
	RoleType:        constants.RolePolecat,
	ExitType:        "DEFERRED",
	CleanupStatus:   "has_unpushed",
	HookBead:        "gt-hook",
	LastSourceIssue: "gt-last",
	AgentState:      "stuck",
}

// seatFactsAnswer is one answer the fake intent-record read gives.
type seatFactsAnswer struct {
	facts seatfacts.Facts
	err   error
}

// agentBeadRetireHost is a host over a temp town holding one agent bead in the
// fake beads database, whose intent-record read is the fake answering answers
// in order, with the daemon's log captured. The shadow lines are remembered
// for the whole daemon process, so the seat's entries are dropped before and
// after the test: a line this test's seat logged in another test must not stand
// in for one it should write here.
func agentBeadRetireHost(t *testing.T, rig, name string, answers []seatFactsAnswer) (*patrolScanHost, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	fields := agentBeadRetireFields
	fields.Rig = rig
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:          beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, rig), rig, name),
		Description: beads.FormatAgentDescription(name, &fields),
	})
	logs := &bytes.Buffer{}
	calls := 0
	h := &patrolScanHost{
		d: &Daemon{
			config:        &Config{TownRoot: town},
			openWorkBeads: bd.open,
			logger:        log.New(logs, "", 0),
		},
		seatFacts: func(string, intent.Seat) (seatfacts.Facts, error) {
			calls++
			if calls > len(answers) {
				t.Fatalf("the seat's record was read %d times, more than the %d the test scripts", calls, len(answers))
			}
			a := answers[calls-1]
			return a.facts, a.err
		},
	}
	seat := rig + "/" + name
	forgetAgentBeadShadow(seat)
	t.Cleanup(func() { forgetAgentBeadShadow(seat) })
	return h, logs
}

// forgetAgentBeadShadow drops the lines one seat has logged in this process,
// leaving other seats' entries to their own tests.
func forgetAgentBeadShadow(seat string) {
	agentBeadShadowMu.Lock()
	defer agentBeadShadowMu.Unlock()
	for key := range agentBeadShadowLogged {
		if key.seat == seat {
			delete(agentBeadShadowLogged, key)
		}
	}
}

// agreeingFacts is the record whose every fact the agent bead answers the same
// way; a row changes the facts it wants to disagree.
func agreeingFacts() seatfacts.Facts {
	return seatfacts.Facts{
		TurnState:     "stuck",
		ExitType:      "DEFERRED",
		CleanupStatus: "has_unpushed",
		WorkBead:      "gt-hook",
		LastWorkBead:  "gt-last",
	}
}

// TestAgentBeadRetireShadow drives AgentRecord over a fake intent-record read
// and holds the shadow log to its contract, one row per case: a disagreement
// logs once per seat, field and value pair in the daemon process and again when
// a value changes; an absent record is compared and logged like any other; an
// unreadable one logs once and never changes the return; a value is made safe
// for one line before it is logged; and a host with no logger writes nothing
// and spends no entry. Every read must return the bead's values.
func TestAgentBeadRetireShadow(t *testing.T) {
	t.Parallel()
	const rig = "retire"
	line := func(seat, body string) string { return agentBeadShadowLogPrefix + rig + "/" + seat + " " + body + "\n" }
	tests := []struct {
		name string
		seat string
		// answers is what the fake record read returns, one per read, and the
		// row makes one read for each.
		answers []seatFactsAnswer
		// loggerFromRead is the first read, counting from 1, made with the host's
		// logger attached; 0 means every read has one. Before it the host has
		// none, as a host without one would.
		loggerFromRead int
		want           string
	}{
		{
			name:    "an agreeing record logs nothing",
			seat:    "shadow-agreeing",
			answers: []seatFactsAnswer{{facts: agreeingFacts()}},
		},
		{
			name: "a disagreement logs once however many reads find it",
			seat: "shadow-once",
			answers: []seatFactsAnswer{
				{facts: func() seatfacts.Facts { f := agreeingFacts(); f.ExitType = "COMPLETED"; return f }()},
				{facts: func() seatfacts.Facts { f := agreeingFacts(); f.ExitType = "COMPLETED"; return f }()},
			},
			want: line("shadow-once", "exit_type intent=COMPLETED bead=DEFERRED"),
		},
		{
			name: "a changed value logs again",
			seat: "shadow-changed",
			answers: []seatFactsAnswer{
				{facts: func() seatfacts.Facts { f := agreeingFacts(); f.ExitType = "COMPLETED"; return f }()},
				{facts: func() seatfacts.Facts { f := agreeingFacts(); f.ExitType = "ESCALATED"; return f }()},
			},
			want: line("shadow-changed", "exit_type intent=COMPLETED bead=DEFERRED") +
				line("shadow-changed", "exit_type intent=ESCALATED bead=DEFERRED"),
		},
		{
			name:    "an absent record is compared as the zero facts",
			seat:    "shadow-absent",
			answers: []seatFactsAnswer{{}},
			want: line("shadow-absent", "exit_type intent= bead=DEFERRED") +
				line("shadow-absent", "cleanup_status intent= bead=has_unpushed") +
				line("shadow-absent", "work_bead intent= bead=gt-hook") +
				line("shadow-absent", "last_work_bead intent= bead=gt-last") +
				line("shadow-absent", "turn_state intent= bead=stuck"),
		},
		{
			name: "an unreadable record logs once and leaves the bead standing",
			seat: "shadow-broken",
			answers: []seatFactsAnswer{
				{err: errors.New("not a record")},
				{err: errors.New("not a record")},
			},
			want: line("shadow-broken", "intent record unreadable, the bead's values stand: not a record"),
		},
		{
			name: "a value is made safe for one line",
			seat: "shadow-control",
			answers: []seatFactsAnswer{{facts: func() seatfacts.Facts {
				f := agreeingFacts()
				f.ExitType = "COMP\tLETED\nforged"
				return f
			}()}},
			want: line("shadow-control", "exit_type intent=COMP LETED forged bead=DEFERRED"),
		},
		{
			name: "an overlong value is cut",
			seat: "shadow-long",
			answers: []seatFactsAnswer{{facts: func() seatfacts.Facts {
				f := agreeingFacts()
				f.ExitType = strings.Repeat("x", agentBeadShadowValueRunes+50)
				return f
			}()}},
			want: line("shadow-long", "exit_type intent="+strings.Repeat("x", agentBeadShadowValueRunes)+" bead=DEFERRED"),
		},
		{
			name: "a value differing only in space keeps its space on the line",
			seat: "shadow-space",
			answers: []seatFactsAnswer{{facts: func() seatfacts.Facts {
				f := agreeingFacts()
				f.ExitType = " DEFERRED"
				return f
			}()}},
			want: line("shadow-space", "exit_type intent= DEFERRED bead=DEFERRED"),
		},
		{
			name: "a host with no logger writes nothing and spends no entry",
			seat: "shadow-nil-logger",
			answers: []seatFactsAnswer{
				{facts: func() seatfacts.Facts { f := agreeingFacts(); f.ExitType = "COMPLETED"; return f }()},
				{facts: func() seatfacts.Facts { f := agreeingFacts(); f.ExitType = "COMPLETED"; return f }()},
			},
			loggerFromRead: 2,
			// The line the second read writes: the row is red if the logger-less
			// first read spent the entry that would spare it.
			want: line("shadow-nil-logger", "exit_type intent=COMPLETED bead=DEFERRED"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, logs := agentBeadRetireHost(t, rig, tc.seat, tc.answers)
			for read := 1; read <= len(tc.answers); read++ {
				if read >= tc.loggerFromRead {
					h.d.logger = log.New(logs, "", 0)
				} else {
					h.d.logger = nil
				}
				got, err := h.AgentRecord(rig, tc.seat)
				if err != nil {
					t.Fatalf("read %d: AgentRecord: %v", read, err)
				}
				if got != agentBeadRetireBead {
					t.Fatalf("read %d: AgentRecord = %+v, want the bead's %+v", read, got, agentBeadRetireBead)
				}
			}
			if got := logs.String(); got != tc.want {
				t.Fatalf("logged %q, want %q", got, tc.want)
			}
			if n := strings.Count(logs.String(), "\n"); n != strings.Count(tc.want, "\n") {
				t.Fatalf("logged %d lines, want %d: %q", n, strings.Count(tc.want, "\n"), logs.String())
			}
		})
	}
}
