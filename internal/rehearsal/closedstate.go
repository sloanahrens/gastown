package rehearsal

import (
	"context"
	"fmt"
)

// closedState returns the state function a run gives Wait for t's beads. A bead
// is done only when its status reads closed: an open, empty or unknown status
// is not done. A failed read returns false with its error wrapped, so Wait
// keeps the bead open and counts the failure.
func closedState(t Town) func(ctx context.Context, id string) (bool, error) {
	return func(ctx context.Context, id string) (bool, error) {
		b, err := t.Bead(ctx, id)
		if err != nil {
			return false, fmt.Errorf("closed state: %w", err)
		}
		return b.Status == "closed", nil
	}
}
