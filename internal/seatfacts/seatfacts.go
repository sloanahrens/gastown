// Package seatfacts reads the facts a seat's intent record holds and compares
// them with the agent bead the same facts are mirrored onto. A reader that
// decides something about a seat (restart it, count it against a cap, end its
// sandbox) is moved from the bead to the record only after the shadow window
// shows the two sources agree, and this package is both halves of that
// comparison (docs/plans/2026-11-15-agent-bead-retirement-plan.md, P1.5).
//
// It is a leaf: it imports internal/intent and internal/patrolscan, neither of
// which may import it back.
package seatfacts

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// Facts is what one seat's intent record says about its work and the turn that
// ended on it: the part of the record that the agent bead mirrors today.
type Facts struct {
	// TurnState is the record's turn_state — done, stuck or awaiting-gate —
	// and "" when no turn's facts are recorded.
	TurnState string
	// ExitType is the gt done exit status the turn ended with.
	ExitType string
	// CleanupStatus is the git state the seat last reported.
	CleanupStatus string
	// WorkBead is the bead waiting to land: the agent bead's hook_bead.
	WorkBead string
	// LastWorkBead is the bead the last turn ran on: the agent bead's
	// last_source_issue.
	LastWorkBead string
	// Desired is the record's desired state, with an absent value read as
	// run, as intent.Record.EffectiveDesired reads it.
	Desired intent.Desired
	// Held reports whether nothing may kill or restart the seat.
	Held bool
}

// Read returns the facts of the seat's intent record under townRoot. It fails
// closed, the way every intent reader does: a record that exists but cannot be
// read or parsed returns the held facts and the error, never facts a caller
// could read as a free seat. A seat with no record is the zero facts — desired
// run, not held — and no error.
func Read(townRoot string, s intent.Seat) (Facts, error) {
	rec, err := intent.Read(townRoot, s)
	if err != nil {
		return Facts{Desired: intent.DesiredPark, Held: true},
			fmt.Errorf("reading seat facts for %s: %w", s, err)
	}
	return Facts{
		TurnState:     rec.TurnState,
		ExitType:      rec.ExitType,
		CleanupStatus: rec.CleanupStatus,
		WorkBead:      rec.WorkBead,
		LastWorkBead:  rec.LastWorkBead,
		Desired:       rec.EffectiveDesired(),
		Held:          rec.Held(),
	}, nil
}

// Disagreement is one fact the intent record and the agent bead answer
// differently.
type Disagreement struct {
	// Field names the fact in the intent record's vocabulary, the one that
	// survives the bead's retirement, so a shadow log line says which of the
	// reader's facts will change when the reader moves onto the record.
	Field string
	// Intent is the value read from the intent record.
	Intent string
	// Bead is the value read from the agent bead.
	Bead string
}

// Disagreements returns every fact on which the intent record and the agent
// bead differ, in a fixed order: exit_type, cleanup_status, work_bead (the
// bead's hook_bead), last_work_bead (the bead's last_source_issue) and
// turn_state (the bead's agent_state). Two equal records — including two empty
// ones — return nothing.
//
// TurnState is compared with the bead's agent_state only when the two are the
// same fact: when the bead's state is one a turn ends in (done, stuck or
// awaiting-gate) or when the record has a turn_state to compare. A bead in
// another state (working, paused, spawning, ...) records something a finished
// turn does not, so it is left alone unless the record says a turn has ended —
// a record whose turn ended while the bead still says the seat is working is
// exactly the drift the shadow window exists to find.
//
// Values are compared as written. A value that differs only in case or
// surrounding space is a disagreement, because moving a reader onto the record
// would change the value it reads.
func Disagreements(f Facts, a patrolscan.AgentRecord) []Disagreement {
	var out []Disagreement
	compare := func(field, fromIntent, fromBead string) {
		if fromIntent != fromBead {
			out = append(out, Disagreement{Field: field, Intent: fromIntent, Bead: fromBead})
		}
	}
	compare("exit_type", f.ExitType, a.ExitType)
	compare("cleanup_status", f.CleanupStatus, a.CleanupStatus)
	compare("work_bead", f.WorkBead, a.HookBead)
	compare("last_work_bead", f.LastWorkBead, a.LastSourceIssue)
	if f.TurnState != "" || isTurnEndState(a.State) {
		compare("turn_state", f.TurnState, a.State)
	}
	return out
}

// isTurnEndState reports whether an agent bead's agent_state is one a turn
// ends in. The comparison ignores case and surrounding space, the way
// patrolscan reads the same field when it asks whether a seat stopped on an
// escalation.
func isTurnEndState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "done", "stuck", "awaiting-gate":
		return true
	}
	return false
}
