package beads

import (
	"errors"
	"fmt"
	"strings"
)

// ErrCloseRefused marks issues a close left open because bd refused them: an
// open child or blocker, or an assignee other than the actor. A
// *PartialCloseError wraps it.
var ErrCloseRefused = errors.New("close refused")

// PartialCloseError reports a batch close that closed some of the issues it
// named and left the rest open. bd 1.2 skips a refused issue in a
// multi-issue close and still exits 0, so a nil error from bd does not mean
// every issue closed; Close re-reads the batch and returns this error when
// any issue is still open. errors.Is(err, ErrCloseRefused) holds for it.
type PartialCloseError struct {
	// Closed are the issues the batch closed (or found already closed).
	Closed []string
	// NotClosed are the issues still open after the batch.
	NotClosed []string
	// Err is why NotClosed stayed open; ErrCloseRefused when bd skipped
	// them.
	Err error
}

func (e *PartialCloseError) Error() string {
	cause := ErrCloseRefused
	if e.Err != nil {
		cause = e.Err
	}
	return fmt.Sprintf("closed %d of %d issues; not closed: %s: %v",
		len(e.Closed), len(e.Closed)+len(e.NotClosed), strings.Join(e.NotClosed, ", "), cause)
}

func (e *PartialCloseError) Unwrap() error {
	if e.Err == nil {
		return ErrCloseRefused
	}
	return e.Err
}

// ClosedIDs returns the issues of ids a close that returned err actually
// closed: all of them when err is nil, the Closed list of a
// *PartialCloseError, and none for any other error.
func ClosedIDs(ids []string, err error) []string {
	if err == nil {
		return ids
	}
	var pe *PartialCloseError
	if errors.As(err, &pe) {
		return pe.Closed
	}
	return nil
}
