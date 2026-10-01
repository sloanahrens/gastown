package beads

import (
	"context"
	"errors"
)

// NewUnavailable is NewWithBeadsDir for a bd that is not there: every call
// fails, as it does with no bd or no database, and nothing is started. Tests
// that only need the bd-backed layer to read as empty hand it to a Rig or a
// caller instead of starting bd.
func NewUnavailable(workDir, beadsDir string) *Beads {
	return newBeads(beadsFields{workDir: workDir, beadsDir: beadsDir, exec: unavailableExec})
}

func unavailableExec(context.Context, bdCall) ([]byte, []byte, error) {
	return nil, nil, errors.New("bd unavailable")
}
