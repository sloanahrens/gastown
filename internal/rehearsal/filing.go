package rehearsal

import (
	"context"
	"fmt"
)

// FileSet files one bead per task, in task order, and returns the ids in the
// same order. On a failed create it stops and returns the ids filed so far
// with the wrapped error, so the caller can close those beads.
func FileSet(ctx context.Context, t Town, tasks []Task) ([]string, error) {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		id, err := t.Create(ctx, task)
		if err != nil {
			return ids, fmt.Errorf("file %s: %w", task.Title, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// CloseOpen closes each id in order with reason. It stops at the first failed
// close and returns that error.
func CloseOpen(ctx context.Context, t Town, ids []string, reason string) error {
	for _, id := range ids {
		if err := t.Close(ctx, id, reason); err != nil {
			return err
		}
	}
	return nil
}
