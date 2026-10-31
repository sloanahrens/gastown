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

	// A failed read here is not fatal, but it must not pass as a safe one:
	// the zero value blocks the move in Decide, so a clone whose state cannot
	// be read is left where it is.
	clean, cleanErr := g.Clean()
	state.Clean = cleanErr == nil && clean

	behind, err := g.Behind()
	if err != nil {
		return Report{}, fmt.Errorf("count the commits behind origin/main: %w", err)
	}
	state.Behind = behind

	ancestor, ancestorErr := g.HeadIsAncestorOfOriginMain()
	state.HeadIsAncestor = ancestorErr == nil && ancestor

	report := Decide(state)
	// Decide's lines say what blocked the move; when the blocker was a failed
	// read, say so rather than claim a state the read never saw. The checks
	// follow Decide's order, so only the read that decided the block is
	// named.
	onMainBehind := state.FetchOK && state.Behind > 0 && state.Branch == mainBranch
	switch {
	case cleanErr != nil && onMainBehind:
		report.Message = fmt.Sprintf("behind %d but the clone's changes could not be read so nothing moved", report.Behind)
	case ancestorErr != nil && onMainBehind && state.Clean:
		report.Message = fmt.Sprintf("behind %d but main's ancestry could not be read so nothing moved", report.Behind)
	}

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
