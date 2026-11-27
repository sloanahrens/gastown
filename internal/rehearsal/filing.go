package rehearsal

import (
	"context"
	"errors"
	"fmt"
)

// FileSet files one bead per task, in task order, and returns the ids in that
// order; a failed create closes the beads filed before it, so no bead of a
// failed filing stays open, and returns the error, joined with any cleanup
// close error.
func FileSet(ctx context.Context, t Town, tasks []Task) ([]string, error) {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		id, err := t.Create(ctx, task)
		if err != nil {
			cause := fmt.Errorf("file %s: %w", task.Title, err)
			return nil, errors.Join(cause, CloseOpen(ctx, t, ids, "filing failed"))
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// CloseOpen closes each id in order with reason and returns the first error,
// still closing the ids after a failed one.
func CloseOpen(ctx context.Context, t Town, ids []string, reason string) error {
	var firstErr error
	for _, id := range ids {
		if err := t.Close(ctx, id, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
