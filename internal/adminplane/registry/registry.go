// Package registry is the workspace state machine of the admin plane (plan row
// V1, gt-76jjj): the states a hosted workspace moves through, the moves the
// rules allow between them, and the Event each allowed move produces.
//
// Nothing calls it yet. Its callers are the console's read views and state
// actions (plan row V2) and the owner's pause and resume (plan row V4), which
// are separate beads. The package holds no database and does no I/O, so the
// rules are testable without either; the Postgres table that stores a
// Workspace and its Events is plan row V1b.
package registry

import (
	"errors"
	"fmt"
	"time"
)

// State is where a workspace stands: Active while it serves its owner, Paused
// or Suspended while it does not, and Closed once it will not again.
type State string

// The states of a workspace. Closed is terminal: no move leaves it.
const (
	Active    State = "active"
	Paused    State = "paused"
	Suspended State = "suspended"
	Closed    State = "closed"
)

// Workspace is what a Transition moves: the workspace's identity and the state
// it is in now. OrgID is the organization that owns it and OwnerSub the owner's
// Google subject, which is the account an organization's membership keys on.
type Workspace struct {
	ID        string
	OrgID     string
	OwnerSub  string
	State     State
	CreatedAt time.Time
}

// Clock is the time source a Transition stamps its Event with. It is injected
// because the event time is the caller's, and a test that read the wall clock
// could not assert what a stored Event says.
type Clock func() time.Time

// Event is one recorded move: the workspace it moved, the actor that asked, the
// state it left and entered, and when. The admin plane stores Events for the
// audit and action logs; this package only makes them.
type Event struct {
	WorkspaceID string
	Actor       string
	From        State
	To          State
	At          time.Time
}

// TransitionError is a refused move. It names the state the workspace was in and
// the state the caller asked for, so the refusal reads without the request that
// caused it.
type TransitionError struct {
	From State
	To   State
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("workspace cannot move from %q to %q", e.From, e.To)
}

// ErrNoClock is Transition's refusal of a nil clock. A move recorded with a
// time nobody supplied would read as if it happened when it was stored.
var ErrNoClock = errors.New("registry: Transition needs a clock")

// moves is the whole rule: the state a workspace leaves, and the states it may
// enter from there. A state named in no key has no move out of it, which is how
// Closed stays terminal and how a state this package does not know is refused
// rather than allowed by default.
var moves = map[State][]State{
	Active:    {Paused, Suspended, Closed},
	Paused:    {Active, Suspended, Closed},
	Suspended: {Active, Closed},
}

// Transition moves w to state to on behalf of actor and returns the workspace in
// its new state with the Event that records the move. A move the rules refuse
// returns w unchanged and a *TransitionError naming both states.
//
// Suspended to Active is the operator's alone, but who may call Transition is
// the authorization bead's decision (plan row V4), so this machine records the
// actor verbatim and judges nothing about it.
func Transition(clock Clock, w Workspace, to State, actor string) (Workspace, Event, error) {
	if clock == nil {
		return w, Event{}, ErrNoClock
	}
	if !legal(w.State, to) {
		return w, Event{}, &TransitionError{From: w.State, To: to}
	}
	from := w.State
	w.State = to
	return w, Event{
		WorkspaceID: w.ID,
		Actor:       actor,
		From:        from,
		To:          to,
		At:          clock(),
	}, nil
}

// legal is the whole refusal rule for one move: from must have a row and to
// must be in it, so no state is legal by default.
func legal(from, to State) bool {
	for _, s := range moves[from] {
		if s == to {
			return true
		}
	}
	return false
}
