package sling

import (
	"errors"
	"fmt"
)

// ErrSeatBranchRefused reports that the seat's own worktree branch refused the
// hook after the spawn, so the dispatch gave the seat back and deferred the bead.
var ErrSeatBranchRefused = errors.New("the seat's worktree branch refused the hook")

// seatBranchRefusal carries a seat-branch refusal's text unchanged and matches
// ErrSeatBranchRefused, so a caller can tell it apart from the other refusals a
// dispatch raises after the spawn: the sling give-back hands the respawn count
// back for a refused seat and not for a held bead (gt-znvgef).
type seatBranchRefusal struct {
	text string
}

func (e *seatBranchRefusal) Error() string { return e.text }

func (e *seatBranchRefusal) Is(target error) bool { return target == ErrSeatBranchRefused }

// newSeatBranchRefusal formats a seat-branch refusal. The text keeps the
// dispatcher's marker first, as a plain fmt.Errorf did, so an automatic
// dispatcher still defers the bead and asks again next tick.
func newSeatBranchRefusal(format string, args ...any) error {
	return &seatBranchRefusal{text: fmt.Sprintf(format, args...)}
}
