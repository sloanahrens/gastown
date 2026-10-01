package beads

import (
	"context"
	"errors"
)

// UnavailableBD is a BDRunner for a bd that is not there: every call fails, as
// it does with no bd or no database. Tests that only need the bd-backed layer
// to read as empty hand it to a Rig or a wrapper instead of starting bd.
var UnavailableBD BDRunner = func(context.Context, BDCall) ([]byte, []byte, error) {
	return nil, nil, errors.New("bd unavailable")
}
