package trackerclient

import (
	"errors"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

// mapped is an API error that also answers errors.Is for a beads sentinel, so
// callers written against bd keep working and the typed API error stays
// reachable through Unwrap.
type mapped struct {
	sentinel error
	cause    error
}

func (m mapped) Error() string        { return m.cause.Error() }
func (m mapped) Unwrap() error        { return m.cause }
func (m mapped) Is(target error) bool { return target == m.sentinel }

// mapErr turns the tracker's typed errors into what code written against bd
// expects: a missing issue is beads.ErrNotFound. Every other kind (a refused
// guard, a conflict, an invalid request, an unavailable service) stays the
// API error, which is a refusal and never an ErrNotFound.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, api.ErrNotFound) {
		return mapped{sentinel: beads.ErrNotFound, cause: err}
	}
	return err
}
