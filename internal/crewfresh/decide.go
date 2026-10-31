// Package crewfresh turns what git said about a crew clone into a report, and
// allows one action: fast-forwarding local main to origin/main when the clone
// is on main, clean and a strict ancestor of origin/main (gt-g4p2j).
package crewfresh

import "fmt"

// mainBranch is the branch a crew clone's work is based on, and the only
// branch this package moves.
const mainBranch = "main"

// Action is the ref move a report allows: none, or a fast-forward of local
// main to origin/main.
type Action int

const (
	// ActionNone leaves every ref where it is.
	ActionNone Action = iota
	// ActionFastForward moves local main forward to origin/main.
	ActionFastForward
)

// State is what a fetch and three git queries said about one crew clone.
type State struct {
	// FetchOK reports whether the fetch succeeded, so origin/main and the
	// counts below are current.
	FetchOK bool
	// Branch is the checked-out branch, "" for a detached HEAD.
	Branch string
	// Clean reports whether the working tree has no changes.
	Clean bool
	// CleanUnreadable reports that Clean could not be read, so Clean holds no
	// reading and the move is blocked.
	CleanUnreadable bool
	// Behind counts the commits origin/main has that HEAD lacks.
	Behind int
	// HeadIsAncestor reports whether HEAD is an ancestor of origin/main, so a
	// fast-forward would lose no commit.
	HeadIsAncestor bool
	// AncestryUnreadable reports that HeadIsAncestor could not be read, so it
	// holds no reading and the move is blocked.
	AncestryUnreadable bool
}

// Report is the decision for one clone: the move it allows, and the line a
// person reads.
type Report struct {
	// Action is the move the caller may make; ActionNone moves nothing.
	Action Action
	// Behind is origin/main's lead over HEAD, as of the last successful fetch.
	Behind int
	// Message names the case, in one of the fixed forms the tests pin.
	Message string
}

// Decide returns the report for a clone in state s, allowing a fast-forward
// only when the fetch succeeded, the clone is on main and clean, and HEAD is a
// strict ancestor of origin/main; a state the clone could not report blocks the
// move and is named.
func Decide(s State) Report {
	r := Report{Behind: s.Behind}
	switch {
	case !s.FetchOK:
		// A failed fetch leaves the counts stale, so no moves are decided on
		// them.
		r.Message = "fetch failed so the numbers are as of the last fetch"
	case s.Behind <= 0:
		r.Message = "up to date"
	case s.Branch != mainBranch:
		r.Message = fmt.Sprintf("behind %d but on %s so main is left alone", s.Behind, branchPhrase(s.Branch))
	case s.CleanUnreadable:
		r.Message = fmt.Sprintf("behind %d but the clone's changes could not be read so nothing moved", s.Behind)
	case !s.Clean:
		r.Message = fmt.Sprintf("behind %d but the clone has local changes so nothing moved", s.Behind)
	case s.AncestryUnreadable:
		r.Message = fmt.Sprintf("behind %d but main's ancestry could not be read so nothing moved", s.Behind)
	case !s.HeadIsAncestor:
		r.Message = fmt.Sprintf("behind %d but main has local commits so nothing moved", s.Behind)
	default:
		r.Action = ActionFastForward
		r.Message = fmt.Sprintf("behind %d and moved", s.Behind)
	}
	return r
}

// branchPhrase names the checked-out branch for a message, and says "no
// branch" rather than leaving a gap for a detached HEAD.
func branchPhrase(branch string) string {
	if branch == "" {
		return "no branch"
	}
	return "branch " + branch
}
