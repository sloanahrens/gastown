package beadsql

import (
	"context"
	"fmt"
	"time"
)

// The events table is the one bd table gastown deletes from with SQL: bd has
// no verb that removes audit rows, the table is dolt_ignored so a DELETE
// creates no Dolt commit, and nothing is derived from an old row (gt-hk21d).
// The statements stay here so the table text lives with every other statement
// gastown sends, and the read-only guard keeps every other DB call as it was.

// countEventsBefore counts the connected database's events rows older than
// cutoff.
const countEventsBefore = "SELECT COUNT(*) AS cnt FROM events WHERE created_at < ?"

// deleteEventsBefore deletes up to the bound limit of the connected database's
// oldest events rows, each older than the bound cutoff. The bound limit is what
// keeps one pass to a transaction the Dolt server tolerates.
const deleteEventsBefore = "DELETE FROM events WHERE created_at < ? ORDER BY created_at LIMIT ?"

// CountEventsBefore counts the connected database's events rows created before
// cutoff.
func (d *DB) CountEventsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	if err := d.QueryRowContext(ctx, countEventsBefore, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("count events older than %s: %w", cutoff, err)
	}
	return n, nil
}

// DeleteEventsBefore deletes up to limit events rows created before cutoff and
// returns how many it removed; limit must be positive so a pass is never
// unbounded.
func (d *DB) DeleteEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("delete events older than %s: limit %d is not positive", cutoff, limit)
	}
	res, err := d.db.ExecContext(ctx, deleteEventsBefore, cutoff, int64(limit))
	if err != nil {
		return 0, fmt.Errorf("delete events older than %s: %w", cutoff, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted events: %w", err)
	}
	return n, nil
}
