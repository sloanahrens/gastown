package crewfresh

import (
	"fmt"
	"time"
)

// Git is the set of questions Run asks one crew clone, and the one move it may
// make.
type Git interface {
	// Fetch updates origin/main, giving up after timeout.
	Fetch(timeout time.Duration) error
	// Branch is the checked-out branch, "" for a detached HEAD.
	Branch() (string, error)
	// Clean reports whether the working tree has no changes.
	Clean() (bool, error)
	// Behind counts the commits origin/main has that HEAD lacks.
	Behind() (int, error)
	// HeadIsAncestorOfOriginMain reports whether a fast-forward would lose no
	// commit.
	HeadIsAncestorOfOriginMain() (bool, error)
	// FastForwardMain moves local main forward to origin/main.
	FastForwardMain() error
}

// Run asks g about one crew clone, makes the move Decide allows, and returns
// the report to show.
//
// Run returns an error, and the zero Report, only when the clone cannot be read
// far enough to decide: Branch or Behind failing. A failed fetch, an unreadable
// Clean or HeadIsAncestorOfOriginMain, and a refused FastForwardMain all come
// back in the Report; a refused fast-forward is returned as an error too, with
// Action set to ActionNone.
func Run(g Git, timeout time.Duration) (Report, error) {
	var state State

	// A fetch that fails is a report, not an error: the counts below are the
	// ones the last successful fetch left behind.
	state.FetchOK = g.Fetch(timeout) == nil

	// Branch and Behind failing mean the clone cannot be read at all, and the
	// error return is the only answer that does not pass as a reading.
	branch, err := g.Branch()
	if err != nil {
		return Report{}, fmt.Errorf("read the checked-out branch: %w", err)
	}
	state.Branch = branch

	// A failed read here is not fatal, but it must not pass as a safe one: the
	// unreadable flag blocks the move in Decide, so a clone whose state cannot
	// be read is left where it is.
	clean, cleanErr := g.Clean()
	state.Clean = cleanErr == nil && clean
	state.CleanUnreadable = cleanErr != nil

	behind, err := g.Behind()
	if err != nil {
		return Report{}, fmt.Errorf("count the commits behind origin/main: %w", err)
	}
	state.Behind = behind

	ancestor, ancestorErr := g.HeadIsAncestorOfOriginMain()
	state.HeadIsAncestor = ancestorErr == nil && ancestor
	state.AncestryUnreadable = ancestorErr != nil

	report := Decide(state)
	if report.Action != ActionFastForward {
		return report, nil
	}
	if err := g.FastForwardMain(); err != nil {
		report.Action = ActionNone
		report.Message = fmt.Sprintf("behind %d but the fast-forward failed so main did not move", report.Behind)
		return report, fmt.Errorf("fast-forward main to origin/main: %w", err)
	}
	return report, nil
}
