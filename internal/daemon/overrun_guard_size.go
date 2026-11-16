package daemon

import "fmt"

// addedLinesCounter is a git handle that can count the lines its worktree adds
// over a base. daemonGit does not declare it, so a handle that cannot count is
// refused where it is asked rather than widening the seam every handle
// implements (gt-3y4bol).
type addedLinesCounter interface {
	AddedLinesSince(base string) (int, error)
}

// overrunSeatAddedLines counts the lines the seat's handle g adds over base, so
// the overrun guard can weigh how much work a seat has done. g is the handle
// overrunSeatGit returned, labeled with the worktree it runs on; the count
// comes through that handle, so it reads the same worktree the save, dirty
// check, push and reset do. A handle that cannot count added lines is an error
// with no count, never a zero: a zero reads as a seat that added nothing.
func overrunSeatAddedLines(g daemonGit, base string) (int, error) {
	counter, ok := g.(addedLinesCounter)
	if !ok {
		return 0, fmt.Errorf("the seat's git handle %T cannot count added lines", g)
	}
	return counter.AddedLinesSince(base)
}
