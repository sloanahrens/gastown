package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// Stats counts the durable issues (wisps are not counted) of rig, or of every
// rig when rig is empty, by status. Closed issues are counted.
func (p *Postgres) Stats(ctx context.Context, rig string) (api.Stats, error) {
	rows, err := p.pool.Query(ctx, "SELECT status, count(*) FROM tracker.issues WHERE NOT ephemeral AND ($1 = '' OR rig = $1) GROUP BY status", rig)
	if err != nil {
		return api.Stats{}, fmt.Errorf("store: stats: %w", err)
	}
	defer rows.Close()
	out := api.Stats{ByStatus: map[string]int{}}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return api.Stats{}, fmt.Errorf("store: stats: %w", err)
		}
		out.ByStatus[status] = n
		out.Total += n
	}
	return out, rows.Err()
}

// ConfigGet returns the value of a configuration key; ok is false when unset.
func (p *Postgres) ConfigGet(ctx context.Context, key string) (string, bool, error) {
	if key == "" {
		return "", false, api.Invalid("a configuration key is required")
	}
	var v string
	err := p.pool.QueryRow(ctx, "SELECT value FROM tracker.config WHERE key = $1", key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: config get: %w", err)
	}
	return v, true, nil
}

// ConfigSet sets a configuration key, replacing any earlier value.
func (p *Postgres) ConfigSet(ctx context.Context, key, value string) error {
	if key == "" {
		return api.Invalid("a configuration key is required")
	}
	if _, err := p.pool.Exec(ctx, "INSERT INTO tracker.config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", key, value); err != nil {
		return fmt.Errorf("store: config set: %w", err)
	}
	return nil
}
